// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// dispatch launches the sandbox via the runner and advances run state. On any
// failure it marks the run FAILED and audits — but never returns the failure to
// the create caller (the run row exists and is queryable). The run token is
// passed to the proxy sidecar via ProxyConfig (verifiable, not a usable secret).
// image is the resolved sandbox OCI image (convention image or a devcontainer
// build result). firstGitHubGrantID, when non-nil, is surfaced in sandbox env
// as WARDYN_GITHUB_GRANT_ID so the git-credential helper can request the token
// via the proxy's local mint route without holding the run token directly.
//
// After Exec starts the agent, dispatch launches a DETACHED completion watcher
// goroutine (see startCompletionWatcher): it blocks on Runner.Wait(ref) and,
// when the agent process exits, transitions the run to COMPLETED (exit 0) or
// FAILED (non-zero) and tears the sandbox down — but only if the run is still
// RUNNING, so a concurrent kill/stop is never clobbered.
//
// INTERACTIVE MODE: when interactive is true, dispatch does CreateSandbox + set
// RUNNING but SKIPS the agent Exec entirely (no `claude -p`) and does NOT start
// the completion watcher (there is no agent process to wait on — the watcher
// would otherwise mark the idle run COMPLETED the moment Wait failed). The
// sandbox comes up idle (the container holds open via `sleep infinity`) so a
// human can `wardyn attach <id>` and drive it. A non-interactive run is
// unchanged. Pair an interactive run with a never-reap policy (AutoStopAfterSec
// < 0) or the idle reaper will stop the idle sandbox.
func (s *Server) dispatch(ctx context.Context, run types.AgentRun, runToken, image string, policy types.RunPolicySpec, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string, injections []runner.InjectionGrant, interactive bool, taskMode string) {
	s.dispatchWithVerify(ctx, run, runToken, image, policy, firstGitHubGrantID, gitPATGrants, sshGrants, injections, interactive, taskMode, nil)
}

// dispatchWithVerify is dispatch plus an optional verify-plan (JSON
// []workspacescan.SetupCommand). When present, the run is a VERIFY run: it execs
// wardyn-verify (in the built devcontainer image) instead of the scanner or the
// agent, and the commands ride WARDYN_VERIFY_COMMANDS. A verify run still sets
// WorkspaceID (for the trusted result linkage), so verifyPlan is the
// discriminator between scan-only and verify-only in the same dispatch.
func (s *Server) dispatchWithVerify(ctx context.Context, run types.AgentRun, runToken, image string, policy types.RunPolicySpec, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string, injections []runner.InjectionGrant, interactive bool, taskMode string, verifyPlan json.RawMessage) {
	// Client-disconnect isolation (M26): dispatch is invoked synchronously from the
	// create-run handler, so a client disconnect cancels ctx mid-flight — which would
	// also fail the compensating StopSandbox below on the same dead ctx and orphan a
	// live sandbox. Detach from cancellation (values preserved) so the whole
	// provision → CAS → compensate sequence always completes. The completion watcher
	// already runs on BaseCtx, not ctx.
	ctx = context.WithoutCancel(ctx)

	// KILL-RACE GUARD (entry): claim PENDING->STARTING conditionally. A
	// POST /runs/{id}/kill landing in the pre-dispatch window (grant writes, the
	// ListRuns scan, a minutes-long devcontainer build) CASes PENDING->KILLED and
	// tears down identity/broker. A blind ->STARTING write here would RESURRECT that
	// killed run: the later STARTING->RUNNING CAS would then apply and the run would
	// boot and execute despite the 202 kill. So if the claim does not apply, the run
	// is no longer PENDING (killed/stopped) — abort without dispatching. Every
	// dispatch caller passes a freshly-created PENDING run.
	claimed, cerr := s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunPending, types.RunStarting)
	if cerr != nil || !claimed {
		data := map[string]any{"note": "run left PENDING by a concurrent kill/stop before dispatch; dispatch aborted"}
		if cerr != nil {
			data["error"] = cerr.Error()
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(data)))
		return
	}

	// CC3 host-eBPF blindness, surfaced AUTOMATICALLY. The host Tetragon sensor
	// cannot see inside a Kata microVM guest, so a CC3 run is blind to the
	// ground-truth stream. wardynd knows the resolved confinement class here, so
	// it records the one-time kernel.sensor.blind audit event itself — making the
	// gap VISIBLE regardless of whether the operator set the sidecar env var
	// WARDYN_GROUNDTRUTH_BLIND_RUNS (that path is kept too). Matches the data
	// shape the sidecar emits (reason="cc3-kata-host-ebpf-blind", run_id) so the
	// downstream audit/correlation is identical.
	if run.ConfinementClass == types.CC3 {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "kernel.sensor.blind",
			run.ID.String(), "success", mustJSON(map[string]any{
				"reason": "cc3-kata-host-ebpf-blind", "run_id": run.ID.String(),
			})))
	}

	// Sandbox env: non-secret values only (invariant 1). The run token never
	// appears here — the proxy holds it via ProxyConfig.RunToken and injects it
	// when forwarding internal API calls from inside the sandbox.
	// Per-run proxy sidecar (docker hostname) unless the config overrides it.
	proxyURL := cmp.Or(s.cfg.ProxyURL, "http://wardyn-proxy:3128")
	sandboxEnv := buildBaseSandboxEnv(run, proxyURL)
	applyDispatchModeEnv(sandboxEnv, run, verifyPlan, interactive, taskMode, firstGitHubGrantID, gitPATGrants, sshGrants)
	applyRepoCloneEnv(sandboxEnv, run, policy)

	// Artifact Repository Redirection (operator-wide site-config): for each
	// configured ecosystem, SUBSTITUTE the corp mirror host for the language's
	// public-registry hosts in this run's egress, deliver the per-tool config
	// (URL-only) into the sandbox, and — for a redirect WITH a token secret —
	// author a proxy-side injection so the token is added on the wire (the sandbox
	// never holds it). No-op when no override is configured. Read once here (the
	// one composition layer every run — agent/verify/record/scan — funnels through).
	// Read the operator-wide site-config ONCE per dispatch. Both consumers below
	// (artifact redirection here + the upstream/corp proxy near ProxyConfig) share
	// this snapshot, so a concurrent admin PUT /api/v1/site-config can never compose
	// a single run from two different snapshots (e.g. new SCM hosts with stale
	// artifact overrides). Store is guaranteed non-nil in dispatch (the run-state
	// CAS transitions below are called unconditionally).
	siteCfg, siteCfgErr := s.cfg.Store.GetSiteConfig(ctx)

	var artifactPlan artifactRedirectPlan
	if siteCfgErr == nil {
		policy.AllowedDomains = substituteArtifactEgress(policy.AllowedDomains, siteCfg)
		artifactPlan = s.planArtifactRedirect(ctx, run, siteCfg)
		for k, v := range artifactPlan.env {
			sandboxEnv[k] = v
		}
		if artifactPlan.configB64 != "" {
			sandboxEnv["WARDYN_ARTIFACT_CONFIG_B64"] = artifactPlan.configB64
		}
	}
	artifactInject := len(artifactPlan.injections) > 0

	// Anthropic auth mode — set on the SANDBOX ENV (not just in agent-run). An
	// INTERACTIVE run never invokes agent-run (the human runs `claude` in the
	// attach shell), so the auth env must live on the container itself or the
	// manual claude session inherits the image's inject-gateway default and is
	// denied. Detect SUBSCRIPTION by the resident ~/.claude bind mount: claude
	// then talks DIRECTLY to api.anthropic.com with its own OAuth creds over the
	// HTTPS_PROXY tunnel, bypassing /wardyn/llm/anthropic (which would deny a run
	// that has no api_key grant to inject). Otherwise (API-key mode) keep the
	// image's gateway default and seed a NON-SECRET placeholder so claude emits a
	// request the proxy strips + re-injects (invariant 1: the real key is never
	// resident; the placeholder is a sentinel, not a credential).
	subscription := specHasMountTarget(&policy, claudeCredTarget)
	// Subscription runs: inject the operator's LIVE OAuth token PROXY-SIDE (the
	// sandbox holds only an inert sentinel) instead of the resident copy, which
	// goes stale — the access token expires (~hours) and the refresh token ROTATES
	// as the operator's own host `claude` refreshes, locking the copy out. This
	// REQUIRES TLS-MITM of api.anthropic.com so the proxy can swap the credential;
	// it is the safe default whenever a token provider is wired. Escape hatch:
	// WARDYN_SUBSCRIPTION_INJECT=off keeps the legacy resident-copy behavior.
	injectSub := subscription && s.cfg.SubscriptionToken != nil && !s.cfg.DisableSubscriptionInject

	// Bedrock: a third Anthropic transport, mutually exclusive with subscription
	// (checked first) and api-key mode (the fallback). See resolveBedrockAuth for
	// the readiness rule and the resident-AWS-cred rationale.
	//
	// modelRun gates Bedrock on a run that actually invokes the model: a verify run
	// (execs wardyn-verify — verifyPlan present) or a scan run (execs wardyn-scan —
	// WorkspaceID set, non-interactive) makes no model call, so it must NOT receive
	// the resident AWS SigV4 creds (least privilege — the creds are masked + confined
	// regardless, but there's no reason to place them in a sandbox that never signs a
	// Bedrock request). Mirrors the WARDYN_VERIFY_ONLY / WARDYN_SCAN_ONLY discriminator.
	modelRun := len(verifyPlan) == 0 && !(run.WorkspaceID != nil && !interactive)
	bedrock := s.resolveBedrockAuth(ctx, run.Agent, subscription, modelRun)
	bedrockReady := bedrock.ready
	// injectBedrockBearer wires bedrock-runtime for proxy-side bearer injection
	// (never-resident); set in the bedrock handling block below and consumed by the
	// CA / injection / MITM-host wiring alongside the subscription path.
	injectBedrockBearer := bedrockReady && bedrock.bearer

	// A HARNESS LOGIN run has no credential yet — its whole purpose is for the
	// operator to run `claude setup-token` in the attach shell and mint one. Point
	// the CLI at the real API (its OAuth flow tunnels to the allowlisted OAuth
	// hosts through HTTPS_PROXY) and seed NO api-key placeholder, so nothing
	// mis-signals api-key mode. No mount, no injection, no MITM.
	harnessLoginRun := run.Task == harnessLoginTask

	// MANAGED subscription: when there is no resident ~/.claude mount and no
	// Bedrock, and the operator connected a Wardyn-managed setup-token, inject it
	// PROXY-SIDE exactly like a resident subscription (the sandbox holds only an
	// inert sentinel). This is the compose-mode subscription path. Precedence:
	// host-staged mount > managed > Bedrock > api-key.
	//
	// OPT-OUT (do NOT override an explicit api-key choice): managed is the FALLBACK
	// when nothing else credentials the run — NOT a silent replacement for an
	// operator who chose api-key. An anthropic api-key grant already present in
	// `injections` (compose's ensureLLMGrant on UseSubscription=false, or a direct
	// api-key run) means the operator opted for api-key; letting managed fire would
	// drop that grant below and silently bill the subscription instead, while the
	// compose review said "api-key". So require no pre-existing anthropic injection.
	// A zero-egress policy (no allow-all, empty allow-list — e.g. a sealed demo
	// sandbox) suppresses the fallback entirely: managed injection APPENDS
	// api.anthropic.com to the allow-list below, and a fallback must not silently
	// widen a policy the operator authored as sealed. Operator-staged subscription
	// mounts are policy-blessed and unaffected.
	managed := !harnessLoginRun && !subscription && !bedrockReady &&
		!hasAnthropicAPIKeyInjection(injections) && s.managedInjectReady(run.Agent) &&
		(policy.AllowAllEgress || len(policy.AllowedDomains) > 0)
	injectManaged := managed

	if harnessLoginRun {
		sandboxEnv["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
	} else if subscription {
		sandboxEnv["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
		// The subscription creds are bind-mounted READ-ONLY at ~/.claude, but
		// claude-code needs a WRITABLE config dir (session-env/, history) — it fails
		// EROFS trying to mkdir under a read-only ~/.claude. Point CLAUDE_CONFIG_DIR at
		// a writable path that agent-run populates from the read-only mount (creds +
		// ~/.claude.json). Set on the sandbox env so BOTH agent-run and an interactive
		// `wardyn attach` shell inherit it.
		sandboxEnv["CLAUDE_CONFIG_DIR"] = "/home/agent/.claude-run"
	} else if managed {
		// Managed subscription (compose, no host ~/.claude mount): same wire posture
		// as resident subscription — talk direct to api.anthropic.com over the tunnel
		// with a writable config dir — but the sentinel creds are DELIVERED via env
		// (WARDYN_CLAUDE_MANAGED_B64) instead of a mount, since there is nothing to
		// mount. agent-run materializes them; the proxy injects the live token.
		sandboxEnv["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
		sandboxEnv["CLAUDE_CONFIG_DIR"] = "/home/agent/.claude-run"
		sandboxEnv["WARDYN_CLAUDE_MANAGED_B64"] = managedSentinelCredsB64()
	} else if bedrockReady {
		for k, v := range bedrock.env {
			sandboxEnv[k] = v
		}
		// Resident SigV4 creds must stay out of PTY/recording streams and any
		// `agent-run --selftest` echo. Bearer mode holds only a placeholder and the
		// ~/.aws-mount mode holds no keys in env at all (the SDK reads the mount), so
		// neither has anything secret to mask here.
		if s.cfg.MaskRegistry != nil && !bedrock.bearer && !bedrock.awsMount {
			s.cfg.MaskRegistry.Add(run.ID, []byte(bedrock.env["AWS_ACCESS_KEY_ID"]))
			s.cfg.MaskRegistry.Add(run.ID, []byte(bedrock.env["AWS_SECRET_ACCESS_KEY"]))
			if tok := bedrock.env["AWS_SESSION_TOKEN"]; tok != "" {
				s.cfg.MaskRegistry.Add(run.ID, []byte(tok))
			}
		}
		for _, h := range bedrock.egressHosts {
			if !domainAllowedExact(policy.AllowedDomains, h) {
				policy.AllowedDomains = append(policy.AllowedDomains, h)
			}
		}
		detail := "resident AWS SigV4 credentials in sandbox env (SigV4 request signing can't be proxy-injected like a static api key); IAM least-privilege scoping is the operator's responsibility"
		mode := "resident"
		switch {
		case bedrock.bearer:
			detail = "bearer token injected proxy-side into bedrock-runtime (TLS-MITM); sandbox holds only a placeholder — never resident"
			mode = "bearer"
		case bedrock.awsMount:
			detail = "host ~/.aws bind-mounted read-only; the AWS SDK resolves credentials (incl. auto-refreshing SSO) from the mount — no static keys stored, none resident in env"
			mode = "aws-dir-mount"
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm.bedrock",
			run.ID.String(), "success", mustJSON(map[string]any{
				"region": s.cfg.BedrockRegion, "model": s.cfg.BedrockModel, "hosts": bedrock.egressHosts,
				"mode": mode, "detail": detail,
			})))
	} else {
		sandboxEnv["ANTHROPIC_API_KEY"] = "wardyn-proxy-injected"
	}
	// Operator model pin: force a specific Anthropic model (e.g. "opus") so the
	// agent doesn't fall back to the account/CLI default (a promo can push that to
	// a cheaper model like Fable). Off unless configured; Claude agent only; never
	// overrides the Bedrock model id (that IS the pin, in inference-profile form).
	if s.cfg.AgentAnthropicModel != "" && run.Agent == "claude-code" && !bedrockReady {
		sandboxEnv["ANTHROPIC_MODEL"] = s.cfg.AgentAnthropicModel
	}

	// Codex (OpenAI) reverse-proxy route: point the OpenAI SDK at the proxy's
	// inspectable /wardyn/llm/openai gateway with a non-secret placeholder; the
	// proxy strips it and injects the brokered OpenAI key (mirrors Anthropic
	// api-key mode). A subscription Codex reaching api.openai.com directly is
	// covered by TLS-MITM when intercept_tls is enabled.
	if run.Agent == "codex-cli" && !subscription {
		sandboxEnv["OPENAI_BASE_URL"] = proxyURL + "/wardyn/llm/openai"
		sandboxEnv["OPENAI_API_KEY"] = "wardyn-proxy-injected"
	}

	// Optional TLS-MITM of opaque LLM CONNECT tunnels (intercept_tls): provision a
	// per-run CA. The PRIVATE key reaches ONLY the proxy sidecar (ProxyConfig,
	// below); the sandbox trusts the PUBLIC cert, installed by agent-run from
	// WARDYN_MITM_CA_PEM and pointed at via NODE_EXTRA_CA_CERTS (additive for Node
	// clients like Claude Code). This makes the subscription-OAuth path inspectable.
	// MITM is provisioned for content inspection (intercept_tls) AND, now, for
	// subscription credential injection (injectSub) — the proxy must terminate the
	// TLS to api.anthropic.com to swap in the live token.
	mitmForInspect := false
	if li := policy.LLMInspection; li != nil && li.InterceptTLS && li.Mode != "" && !strings.EqualFold(li.Mode, "off") {
		mitmForInspect = true
	}
	var mitmCACertPEM, mitmCAKeyPEM string
	if injectSub || injectManaged || mitmForInspect || artifactInject || injectBedrockBearer {
		certPEM, keyPEM, caErr := generateRunCA(time.Now())
		if caErr != nil {
			// CAS from STARTING (claimed at dispatch entry) so a concurrent kill's
			// KILLED state is preserved rather than clobbered back to FAILED.
			s.failAndRevoke(ctx, run.ID, types.RunStarting)
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": "mitm ca: " + caErr.Error()})))
			return
		}
		mitmCACertPEM, mitmCAKeyPEM = string(certPEM), string(keyPEM)
		sandboxEnv["WARDYN_MITM_CA_PEM"] = mitmCACertPEM
		// CA files live under /tmp/wardyn (any-uid-writable, works for images with
		// arbitrary USER/HOME — the old /home/agent/.wardyn pin dangled for
		// envbuilder/BYOI images whose HOME differs; precedent: mainProcCastDir).
		// NODE_EXTRA_CA_CERTS is ADDITIVE (Node keeps its bundled roots), so it
		// points at the bare CA. Everything OpenSSL-shaped REPLACES its trust store
		// via these vars, so they point at the COMBINED bundle (system roots + the
		// per-run CA) that install_mitm_ca/agentIdleScript assemble — the bare CA
		// there would break verification of non-MITM'd CONNECT-tunneled hosts.
		// JVM (keystore) and Deno (DENO_CERT) are documented as not covered.
		sandboxEnv["NODE_EXTRA_CA_CERTS"] = "/tmp/wardyn/mitm-ca.pem"
		sandboxEnv["SSL_CERT_FILE"] = "/tmp/wardyn/ca-bundle.pem"
		sandboxEnv["REQUESTS_CA_BUNDLE"] = "/tmp/wardyn/ca-bundle.pem"
		sandboxEnv["CURL_CA_BUNDLE"] = "/tmp/wardyn/ca-bundle.pem"
	}

	// Subscription / managed: author a re-mintable api_key grant whose SENTINEL
	// secret name resolves to a LIVE Anthropic OAuth token (resident host token, or
	// the Wardyn-managed captured setup-token) rather than a stored secret; append
	// its injection and ensure the exact host is egress-allowed (the injector's hard
	// requirement). Non-approval api_key grants are re-mintable by design, so the
	// proxy re-resolves the token indefinitely. This is what makes the sandbox's
	// sentinel sufficient. injectSub and injectManaged are mutually exclusive by
	// construction (managed requires !subscription).
	if injectSub || injectManaged {
		const anthropicAPIHost = "api.anthropic.com"
		sentinelName := subscriptionOAuthSecret
		injectSource := "subscription"
		detail := "live subscription OAuth token injected proxy-side; sandbox's staged copy holds only inert sentinel tokens (access + refresh both replaced at staging)"
		if injectManaged {
			sentinelName = types.ManagedOAuthSecret
			injectSource = "managed"
			detail = "Wardyn-managed subscription (setup-token) injected proxy-side; sandbox holds only an inert sentinel delivered via env (no host ~/.claude mount)"
		}
		// Subscription/managed REPLACES any api-key injection for the same host. A
		// ceiling that also lists an anthropic-api-key grant (e.g. the composer-dev
		// ceiling) would otherwise leave TWO injections for api.anthropic.com; the
		// proxy resolves both at startup and the api-key mint fails closed when its
		// secret is absent — crashing the sidecar. Drop it here (the direct-run
		// equivalent of reconcileLLMAccess's removeAPIKeyGrantForHost).
		kept := injections[:0]
		for _, ig := range injections {
			if strings.EqualFold(strings.TrimSuffix(ig.Rule.Host, "."), anthropicAPIHost) {
				continue
			}
			kept = append(kept, ig)
		}
		injections = kept
		subGrantID := uuid.New()
		subScope, _ := json.Marshal(map[string]string{
			"host":        anthropicAPIHost,
			"header":      "Authorization",
			"format":      "Bearer %s",
			"secret_name": sentinelName,
		})
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: subGrantID, RunID: run.ID, CreatedAt: time.Now(),
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: subScope, TTLSeconds: 3600},
		}); gerr != nil {
			// CAS from STARTING (claimed at dispatch entry) so a concurrent kill's
			// KILLED state is preserved rather than clobbered back to FAILED.
			s.failAndRevoke(ctx, run.ID, types.RunStarting)
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": injectSource + " inject grant: " + gerr.Error()})))
			return
		}
		if rule, derr := injectionRuleFromScope(subScope); derr == nil {
			injections = append(injections, runner.InjectionGrant{GrantID: subGrantID, Rule: rule})
		}
		if !domainAllowedExact(policy.AllowedDomains, anthropicAPIHost) {
			policy.AllowedDomains = append(policy.AllowedDomains, anthropicAPIHost)
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm.subscription_inject",
			run.ID.String(), "success", mustJSON(map[string]any{
				"host": anthropicAPIHost, "tls_mitm": true, "source": injectSource, "detail": detail,
			})))
	}

	// Bedrock BEARER injection: author an api_key grant whose Authorization: Bearer
	// header injects the operator's Bedrock API key into bedrock-runtime, and mark
	// that host TLS-MITM-eligible for THIS run. This is the same operator-configured
	// MITM-host + paired-injection pattern as corp artifact hosts (isCorpMITMHost) —
	// bedrock-runtime is not a wildcard, the token is the operator's own, and the CA
	// key stays in proxy memory. The sandbox holds only the placeholder bearer.
	var bedrockMITMHosts []string
	if injectBedrockBearer {
		bedrockMITMHosts = append(bedrockMITMHosts, bedrock.runtimeHost)
		beScope, _ := json.Marshal(map[string]string{
			"host":        bedrock.runtimeHost,
			"header":      "Authorization",
			"format":      "Bearer %s",
			"secret_name": bedrockAPIKeySecret,
		})
		beGrantID := uuid.New()
		if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
			ID: beGrantID, RunID: run.ID, CreatedAt: time.Now(),
			Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: beScope, TTLSeconds: 3600},
		}); gerr != nil {
			// CAS from STARTING (claimed at dispatch entry) so a concurrent kill's
			// KILLED state is preserved rather than clobbered back to FAILED.
			s.failAndRevoke(ctx, run.ID, types.RunStarting)
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": "bedrock bearer inject grant: " + gerr.Error()})))
			return
		}
		if rule, derr := injectionRuleFromScope(beScope); derr == nil {
			injections = append(injections, runner.InjectionGrant{GrantID: beGrantID, Rule: rule})
		}
	}

	// Artifact-redirect token injections (authored in planArtifactRedirect, whose
	// egress substitution already added each corp host to policy.AllowedDomains, so
	// the injector's exact-allowlist check passes). Appended AFTER the subscription
	// block, which reslices `injections` in place.
	injections = append(injections, artifactPlan.injections...)

	// Fail CLOSED at schedule time when inspection is REQUIRED but the resolved LLM
	// transport is OPAQUE (M24). Opaque transports: (a) a subscription/OAuth transport
	// that is NOT being MITM'd (injectSub / intercept_tls auto-enable MITM, making it
	// inspectable); (b) SigV4 Bedrock via ~/.aws or resident keys — uninspectable by
	// construction (we cannot re-sign a MITM'd SigV4 request), so only the Bedrock
	// BEARER path (proxy-injected, MITM'd) is inspectable. Previously only the
	// subscription case failed closed, silently exempting opaque Bedrock. The default
	// (require_inspectable_llm=false) instead degrades visibly rather than failing.
	if li := policy.LLMInspection; li != nil && li.RequireInspectableLLM &&
		li.Mode != "" && !strings.EqualFold(li.Mode, "off") &&
		((subscription && !li.InterceptTLS && !injectSub) || (bedrockReady && !bedrock.bearer)) {
		// CAS from STARTING so a concurrent kill's KILLED is not clobbered to FAILED.
		s.failAndRevoke(ctx, run.ID, types.RunStarting)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "require_inspectable_llm: the resolved LLM transport is opaque (subscription without MITM, or SigV4 Bedrock); enable intercept_tls or use an inspectable transport"})))
		return
	}

	// Host bind mounts: copy the resolved POLICY's WorkspaceMounts into the spec.
	// Mounts may be authored on a stored policy (admin-gated policy CRUD) OR
	// INLINE on the create request by an admin / SSO-gated human operator
	// (createRunRequest.InlinePolicy) — both flow through the SAME resolved
	// RunPolicySpec here, so this is still the only path that populates
	// spec.Mounts. They are NEVER chosen by the in-sandbox agent: the agent-run
	// entrypoint has no access to this surface, so a prompt-injected agent can
	// never pick a host mount (invariants 1 & 3). Every mount was already
	// deny-list-validated by runner.ValidateMount at policy-write/inline-validate
	// time (validatePolicySpec); the docker driver re-validates it
	// defense-in-depth at sandbox-create time. runner.ValidateMount is unchanged.
	var mounts []runner.Mount
	for _, wm := range policy.WorkspaceMounts {
		mounts = append(mounts, runner.Mount{
			Source: wm.Source,
			Target: wm.Target,
			// Safe default: omitted read_only => read-only. RW only on explicit
			// read_only=false in the policy.
			ReadOnly: wm.ReadOnlyOrDefault(),
		})
	}

	// Host-mode Bedrock ~/.aws mount (operator config, not agent-chosen; same trust
	// and the same driver deny-list re-validation as the WorkspaceMounts above).
	// READ-ONLY: the sandbox reads the SSO cache / config but can never write to the
	// operator's host AWS state. Only set on the host-mode path (resolveBedrockAuth
	// gated it on BedrockAWSConfigDir existing); never present for a team deployment.
	if bedrockReady && bedrock.awsMount {
		mounts = append(mounts, runner.Mount{
			Source:   bedrock.awsMountSource,
			Target:   sandboxAWSDir,
			ReadOnly: true,
		})
	}

	// Operator-wide upstream/corp proxy (site-config → ProxyConfig.UpstreamProxyURL).
	// A locked-down corporate network may give the sandbox host NO direct internet
	// route at all — site_config.UpstreamProxySecretRef (admin-authored via
	// PUT /api/v1/site-config) names the secret holding the corp CONNECT-proxy URL.
	// The resolved cred-bearing URL lands in the sidecar's WARDYN_PROXY_CONFIG_JSON
	// env var, the SAME posture as RunToken today: proxy-process-only, never on the
	// sandbox side, masked from decision-log/stdout by the proxy — a deliberate,
	// already-documented tradeoff (see runner.ProxyConfig.UpstreamProxyURL), not a
	// new one. Fail SAFE: an unconfigured ref, an unresolvable secret, or a
	// non-http URL all leave this "" (direct egress, today's behavior) plus an
	// audit event; none of them fail the run or crash dispatch.
	upstreamProxyURL := ""
	if siteCfgErr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
			run.ID.String(), "failure", mustJSON(map[string]any{"reason": "site-config-read-error"})))
	} else if siteCfg.UpstreamProxySecretRef != "" {
		var getSecret func(context.Context, string) ([]byte, error)
		if s.cfg.Secrets != nil {
			getSecret = s.cfg.Secrets.Get
		}
		resolved, failReason := resolveUpstreamProxyURL(ctx, siteCfg.UpstreamProxySecretRef, getSecret)
		if failReason != "" {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
				run.ID.String(), "failure", mustJSON(map[string]any{
					"reason": failReason, "secret_ref": siteCfg.UpstreamProxySecretRef,
				})))
		} else {
			upstreamProxyURL = resolved
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.upstream_proxy.resolve",
				run.ID.String(), "success", mustJSON(map[string]any{"secret_ref": siteCfg.UpstreamProxySecretRef})))
		}
	}

	spec := runner.SandboxSpec{
		RunID:            run.ID,
		Image:            image,
		ConfinementClass: run.ConfinementClass,
		Env:              sandboxEnv,
		Mounts:           mounts,
		// Interactive runs come up idle for `wardyn attach`; the driver prepares the
		// workspace (clones the repo into ~/work) on the idle process so the attach
		// shell isn't empty. A non-interactive run's task exec does this itself.
		Interactive: interactive,
		ProxyConfig: runner.ProxyConfig{
			RunToken:        runToken,
			ControlPlaneURL: s.cfg.ControlPlaneURL,
			// The proxy sidecar enforces THIS run's egress policy; a proxy
			// without a policy fails closed (no egress at all).
			Policy:    policy,
			Injection: injections,
			// Per-run TLS-MITM CA (empty unless intercept_tls): private key to the
			// proxy only; the sandbox trusts the public cert via the agent env.
			MITMCACertPEM: mitmCACertPEM,
			MITMCAKeyPEM:  mitmCAKeyPEM,
			// Operator-configured corp artifact hosts the proxy is allowed to
			// TLS-MITM (beyond the built-in LLM hosts) so a registry token injects on
			// the wire. Only hosts with a resolved token injection appear here — a
			// tight per-host allowlist, never a blanket. See isMITMHost widening.
			MITMHosts: append(append([]string{}, artifactPlan.mitmHosts...), bedrockMITMHosts...),
			// MITM the BUILT-IN LLM hosts only when that's actually intended for this
			// run — subscription OAuth injection or intercept_tls content inspection.
			// The CA above may also be minted purely for artifact-token injection, so
			// this keeps an artifact-only run from TLS-terminating a direct CONNECT to
			// Anthropic/OpenAI it never asked to intercept.
			MITMLLM: injectSub || injectManaged || mitmForInspect,
			// Resolved above from site-config.UpstreamProxySecretRef; "" when
			// unconfigured or unresolvable (direct dial, backward-compatible).
			UpstreamProxyURL: upstreamProxyURL,
		},
		// Hard resource caps. A nil policy block (or a zero field) becomes the
		// driver's conservative platform default, so EVERY sandbox is CPU/memory/
		// PID capped even when the policy sets nothing — a fleet of independent
		// agents must not be able to OOM-kill, fork-bomb, or disk-fill the host or
		// each other (C5).
		Resources: resourceLimitsToRunner(policy.Resources),
		Labels: map[string]string{
			"wardyn.run":   run.ID.String(),
			"wardyn.agent": run.Agent,
		},
	}

	sb, err := s.cfg.Runner.CreateSandbox(ctx, spec)
	if err != nil {
		// Conditional: only mark FAILED if still STARTING. A kill landing between the
		// entry claim and this failure moved the run to KILLED — don't clobber that
		// terminal state (mirrors the STARTING->RUNNING guard below).
		s.failAndRevoke(ctx, run.ID, types.RunStarting)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": err.Error()})))
		return
	}
	_ = s.cfg.Store.SetSandboxRef(ctx, run.ID, sb.Ref)

	// KILL-RACE GUARD: advance STARTING->RUNNING CONDITIONALLY. CreateSandbox can
	// be slow (image pull); a concurrent POST /runs/{id}/kill may have moved the
	// run out of STARTING (to KILLED/STOPPED) and already torn down identity +
	// broker while we were creating the sandbox. An unconditional RUNNING write
	// would resurrect a killed run AND leak the just-created container. So if the
	// conditional transition does NOT apply (the run is no longer STARTING), we
	// tear the sandbox we just created back down and stop — never running Exec or
	// the completion watcher. The kill path already revoked identity/broker; we
	// must not undo its work.
	applied, uerr := s.cfg.Store.UpdateRunStateIf(ctx, run.ID, types.RunStarting, types.RunRunning)
	if uerr != nil || !applied {
		// Killed/stopped mid-dispatch (or a store error). Free the orphaned
		// sandbox and bail without resurrecting the run. The note states only what
		// was observed: whether the teardown actually happened is stopSandboxOrAudit's
		// to report, never this event's to assert.
		s.stopSandboxOrAudit(ctx, run.ID, sb.Ref, "run.dispatch")
		data := map[string]any{
			"sandbox_ref": sb.Ref,
			"note":        "run left STARTING by a concurrent kill/stop during CreateSandbox; dispatch aborted",
		}
		if uerr != nil {
			data["error"] = uerr.Error()
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.dispatch",
			run.ID.String(), "failure", mustJSON(data)))
		return
	}

	// INTERACTIVE MODE: skip the agent Exec AND the completion watcher. The
	// sandbox is RUNNING and idle (the container holds open), ready for a human to
	// `wardyn attach`. There is no agent process, so there is nothing for the
	// watcher to Wait on — starting it would have it observe an immediate Wait
	// failure (no tracked agent exec) and could prematurely terminate the run.
	// BYOI runtime preflight: a wrapped arbitrary base is guaranteed to carry the
	// runner tools (the wrap COPYs them), but may still lack a shell or the harness
	// CLI. Run `agent-run --selftest` and observe its exit — for a batch run, fail
	// CLOSED on nonzero (honest FAILED + audit, never a hang or a silent bad run);
	// for an interactive/login box, warn-only (a login box legitimately lacks repo
	// wiring and the human sees the shell regardless). Keyed off the wardyn-byoi/
	// image tag so convention/devcontainer runs are unaffected.
	byoi := strings.HasPrefix(image, "wardyn-byoi/")

	if interactive {
		if byoi {
			s.byoiSelftest(ctx, run, sb.Ref, false /* warn-only */)
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.interactive",
			run.ID.String(), "success", mustJSON(map[string]any{
				"sandbox_ref": sb.Ref,
				"note":        "interactive run: no agent task exec'd; awaiting attach",
			})))
		return
	}

	// When a task is provided, launch the agent process inside the now-running
	// sandbox. The driver wraps the argv with wardyn-rec (recording) when
	// configured. Exec failure: audit + stop the sandbox + mark FAILED.
	if run.Task != "" {
		// BYOI: gate the task exec on a passing selftest (fail closed).
		if byoi && !s.byoiSelftest(ctx, run, sb.Ref, true /* fail-closed */) {
			s.stopSandboxOrAudit(ctx, run.ID, sb.Ref, "run.selftest")
			s.failAndRevoke(ctx, run.ID, types.RunRunning)
			return
		}
		argv := []string{"/usr/local/bin/agent-run", run.Task}
		execID, xerr := s.cfg.Runner.Exec(ctx, sb.Ref, argv)
		if xerr != nil {
			s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
				run.ID.String(), "failure", mustJSON(map[string]any{"error": xerr.Error()})))
			s.stopSandboxOrAudit(ctx, run.ID, sb.Ref, "run.exec")
			// Conditional: a concurrent kill may have moved RUNNING->KILLED; don't
			// clobber it with FAILED.
			s.failAndRevoke(ctx, run.ID, types.RunRunning)
			return
		}
		// Persist the agent exec id so the boot reconciler can observe AGENT liveness
		// (ExecInspect) across a wardynd restart: an idle-container exec run whose
		// agent already exited must finalize + revoke, not strand RUNNING (U008/U039).
		// Best-effort like SetSandboxRef; "" for exec-less substrates (container==agent).
		_ = s.cfg.Store.SetRunAgentExecID(ctx, run.ID, execID)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.exec",
			run.ID.String(), "success", mustJSON(map[string]any{"argv": argv})))

		// Completion tracking: watch the agent process to exit and propagate its
		// outcome. The watcher runs on a DETACHED context (NOT ctx — that is the
		// request context, cancelled when the create-run handler returns, which
		// would kill the watcher immediately). See startCompletionWatcher.
		s.startCompletionWatcher(run.ID, sb.Ref, execID)
	}
}

// buildBaseSandboxEnv assembles dispatchWithVerify's baseline non-secret sandbox
// env (invariant 1: the run token never appears here): proxy routing, the
// toolchain-fidelity proxy config Maven/Gradle need (they ignore HTTP(S)_PROXY),
// and git commit attribution carrying the sub/act delegation chain. Every later
// phase in dispatchWithVerify only adds to this map, never removes from it.
// Extracted verbatim from dispatchWithVerify — pure construction, no branches
// that affect control flow.
func buildBaseSandboxEnv(run types.AgentRun, proxyURL string) map[string]string {
	return map[string]string{
		"WARDYN_RUN_ID":    run.ID.String(),
		"WARDYN_PROXY_URL": proxyURL,
		// Standard proxy env: agents using HTTP_PROXY-aware clients route
		// through the wardyn-proxy automatically (L2 enforcement).
		"HTTP_PROXY":  proxyURL,
		"HTTPS_PROXY": proxyURL,
		// Exclude the proxy itself and loopback from proxy traversal.
		"NO_PROXY": "wardyn-proxy,localhost,127.0.0.1,::1",
		// Toolchain-fidelity env (PLATFORM-wide, every image — not just the fat
		// campaign image). These were live-found in the cross-language campaign:
		//   GOTMPDIR: the sandbox mounts /tmp NOEXEC, but `go test` compiles+EXECS
		//     its test binaries in $TMPDIR → "permission denied". Point it at the
		//     agent's exec-allowed HOME. (Plain env survives a shell; only PATH is
		//     reset by a login shell — which wardyn-verify no longer uses.)
		//   MAVEN_OPTS: Maven ALONE ignores HTTP(S)_PROXY (npm/pip/cargo/go/git
		//     honor it) → "Unknown host repo.maven.apache.org". The JVM proxy
		//     sysprops route Maven through wardyn-proxy. (The fat image also bakes
		//     a settings.xml <proxy> as belt-and-braces.)
		//   GRADLE_OPTS: Gradle is the same JVM-networking case as Maven — it
		//     resolves dependencies via java.net's proxy selector, which reads the
		//     standard -Dhttp(s).proxyHost/-port/-DnonProxyHosts sysprops (Gradle's
		//     own docs point at these same properties, normally set in
		//     gradle.properties — GRADLE_OPTS is the env-expressible equivalent, so
		//     it's the exact same JVM opts string as MAVEN_OPTS). Reused verbatim.
		//   NOT covered here (need image/build-time FILE config, not env, so out of
		//     scope for this platform-env pass): apt (/etc/apt/apt.conf.d/*.conf)
		//     and a per-project gradle.properties/init.d script for repos that
		//     don't launch via the gradle/gradlew wrapper JVM. npm/pip/cargo/go/git
		//     already honor HTTP(S)_PROXY above and need nothing further.
		"GOTMPDIR":    "/home/agent/.gotmp",
		"GOCACHE":     "/home/agent/.cache/go-build",
		"MAVEN_OPTS":  mavenProxyOpts(proxyURL),
		"GRADLE_OPTS": mavenProxyOpts(proxyURL),
		// Git commit attribution: carry the sub/act delegation chain into the commit
		// graph so an agent's commits are traceable to the governed run — AUTHOR is
		// the human who authorized the run (sub), COMMITTER is the agent run (act).
		// git reads these env vars without touching the image. (Deterministic
		// Run-Id/On-Behalf-Of commit trailers need an in-image prepare-commit-msg
		// hook — tracked as a follow-up.)
		"GIT_AUTHOR_NAME":     run.CreatedBy,
		"GIT_AUTHOR_EMAIL":    gitEmailLocal(run.CreatedBy) + "@wardyn.local",
		"GIT_COMMITTER_NAME":  "wardyn-agent:" + run.Agent,
		"GIT_COMMITTER_EMAIL": run.ID.String() + "@agent.wardyn.local",
	}
}

// applyDispatchModeEnv sets dispatchWithVerify's run-mode discriminator env vars
// (verify-only / scan-only / exec task mode) plus the non-secret grant-id maps
// (WARDYN_GITHUB_GRANT_ID / WARDYN_GIT_PAT_GRANTS / WARDYN_SSH_GRANTS) that let
// the in-sandbox helpers mint the credentials they're eligible for. Extracted
// verbatim from dispatchWithVerify — every branch here only decides which keys
// land in sandboxEnv, none of them change dispatchWithVerify's own control flow.
func applyDispatchModeEnv(sandboxEnv map[string]string, run types.AgentRun, verifyPlan json.RawMessage, interactive bool, taskMode string, firstGitHubGrantID *uuid.UUID, gitPATGrants, sshGrants map[string]string) {
	// Governed repo SCAN run: after cloning, the entrypoint runs wardyn-scan (which
	// walks ~/work and PUTs ScanFacts to the brokered scan-results route) INSTEAD of
	// the agent. A non-nil WorkspaceID uniquely marks a scan run (ordinary runs never
	// set it); no agent CLI / model call happens.
	// A verify run (verifyPlan present) execs wardyn-verify in the built image;
	// a scan run (WorkspaceID set, no verify plan) execs wardyn-scan. verifyPlan
	// is the discriminator since both set WorkspaceID for the trusted upload
	// linkage. The approved setup commands are non-secret operator-authored
	// values (secrets are proxy-injected, never resident), so they ride env.
	if len(verifyPlan) > 0 {
		sandboxEnv["WARDYN_VERIFY_ONLY"] = "1"
		sandboxEnv["WARDYN_VERIFY_COMMANDS"] = string(verifyPlan)
	} else if run.WorkspaceID != nil && !interactive {
		// An INTERACTIVE workspace-linked run (interactive Record Mode) is a
		// human-driven sandbox, not a scan — never mark it WARDYN_SCAN_ONLY.
		sandboxEnv["WARDYN_SCAN_ONLY"] = "1"
	}
	// exec task mode (BYOA/CI lane): agent-run runs the task as a plain shell
	// command instead of the agent harness. Only the discriminator rides env —
	// everything above/below (clone, grants, egress, recording) is identical.
	if taskMode == "exec" {
		sandboxEnv["WARDYN_TASK_MODE"] = "exec"
	}
	if firstGitHubGrantID != nil {
		sandboxEnv["WARDYN_GITHUB_GRANT_ID"] = firstGitHubGrantID.String()
	}
	// git_pat grants: surface the {host: grant_id} map so the git-credential
	// helper can mint the stored PAT for a matched non-GitHub host. Non-secret
	// (grant ids, not the PAT); the value is returned only through the brokered mint.
	if len(gitPATGrants) > 0 {
		if b, merr := json.Marshal(gitPATGrants); merr == nil {
			sandboxEnv["WARDYN_GIT_PAT_GRANTS"] = string(b)
		}
	}
	// ssh_key grants: surface the {host: grant_id} map so agent-run can mint the
	// resident SSH private key at clone time (SSH has NO credential-helper seam,
	// so the key is written to a 0400 file and wiped after the clone). Non-secret
	// (grant ids, not the key); the key material is returned only via the brokered
	// mint and never touches env. See GrantSSHKey.
	if len(sshGrants) > 0 {
		if b, merr := json.Marshal(sshGrants); merr == nil {
			sandboxEnv["WARDYN_SSH_GRANTS"] = string(b)
		}
	}
}

// applyRepoCloneEnv surfaces the repo(s) to clone (the legacy single run.Repo
// plus each onboarded WorkspaceRepo on the resolved policy) as sandbox env the
// agent-run launcher reads before running the agent — non-secret; invariant 1
// preserved. Extracted verbatim from dispatchWithVerify — pure map mutation, no
// branch here changes control flow. See buildRepoRecords for the validation
// (repoFieldSafe, allowed-prefix targets, dedup) it relies on.
func applyRepoCloneEnv(sandboxEnv map[string]string, run types.AgentRun, policy types.RunPolicySpec) {
	if slug := strings.TrimSpace(run.Repo); slug != "" && repoFieldSafe(slug) {
		sandboxEnv["WARDYN_REPO_SLUG"] = slug
		if url := repoCloneURL(slug); url != "" {
			sandboxEnv["WARDYN_REPO_URL"] = url
		}
	}
	if repos := buildRepoRecords(run.Repo, policy.WorkspaceRepos); repos != "" {
		sandboxEnv["WARDYN_REPOS"] = repos
	}
}

// hasAnthropicAPIKeyInjection reports whether the run already carries an api_key
// injection targeting api.anthropic.com — i.e. the operator/compose set up the
// api-key transport for Anthropic. The managed-subscription gate uses it to stay
// a FALLBACK (fire only when nothing else credentials Anthropic), never a silent
// override of an explicit api-key choice. Mirrors the drop-loop's host check.
func hasAnthropicAPIKeyInjection(injections []runner.InjectionGrant) bool {
	for _, ig := range injections {
		if strings.EqualFold(strings.TrimSuffix(ig.Rule.Host, "."), "api.anthropic.com") {
			return true
		}
	}
	return false
}

// byoiSelftest runs `agent-run --selftest` inside a BYOI sandbox and waits for
// its exit, auditing the outcome. It relies on the runner's "latest Exec wins"
// contract: this exec is tracked and Wait'd BEFORE the real task exec replaces
// it, so the subsequent task's completion watcher is unaffected. Returns true
// when the selftest passed (exit 0). failClosed only governs the audit tone —
// the caller decides what to do with a false (fail the batch run, or warn-only
// for interactive). A selftest that cannot even start (missing shell/binary,
// exit 127) surfaces as a non-nil Exec/Wait error → returns false.
// byoiSelftestTimeout bounds the fail-closed BYOI selftest gate so a hostile or
// broken base image whose agent-run --selftest hangs cannot block the dispatch
// goroutine forever — on timeout the gate fails closed (returns false).
const byoiSelftestTimeout = 2 * time.Minute

func (s *Server) byoiSelftest(ctx context.Context, run types.AgentRun, ref string, failClosed bool) bool {
	ctx, cancel := context.WithTimeout(ctx, byoiSelftestTimeout)
	defer cancel()
	if _, xerr := s.cfg.Runner.Exec(ctx, ref, []string{"/usr/local/bin/agent-run", "--selftest"}); xerr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"error": xerr.Error(), "fail_closed": failClosed,
				"detail": "BYOI image could not run agent-run --selftest (missing shell or harness binary?)",
			})))
		return false
	}
	code, werr := s.cfg.Runner.Wait(ctx, ref)
	if werr != nil {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"error": werr.Error(), "fail_closed": failClosed,
			})))
		return false
	}
	if code != 0 {
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
			run.ID.String(), "failure", mustJSON(map[string]any{
				"exit_code": code, "fail_closed": failClosed,
				"detail": "BYOI image failed the agent-run contract selftest",
			})))
		return false
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.selftest",
		run.ID.String(), "success", mustJSON(map[string]any{"exit_code": 0})))
	return true
}

// startCompletionWatcher launches the detached goroutine that blocks until the
// agent process exits and then propagates its outcome to the run's durable
// state + audit trail. Invariants it upholds:
//
//   - DETACHED CONTEXT: it derives its context from s.cfg.BaseCtx (the daemon
//     rootCtx), NOT the request/dispatch ctx. The request ctx is cancelled the
//     moment the create-run handler returns, which would otherwise cancel
//     Runner.Wait and the watcher before the agent ever finishes. BaseCtx lives
//     for the daemon's lifetime (cancelled on shutdown).
//   - KILLED-RACE GUARD: the terminal transition is a conditional store update
//     from RUNNING only (UpdateRunStateIf). A user may `wardyn kill` mid-run,
//     moving the run to KILLED and tearing the sandbox down; the watcher must
//     NOT clobber that. If the conditional update does not apply (the run is no
//     longer RUNNING), the watcher does nothing further — in particular it does
//     NOT tear the sandbox down (kill already did).
//   - TEARDOWN ONLY ON WIN: StopSandbox is called only when the watcher won the
//     RUNNING->terminal transition, so resources are freed exactly once and a
//     run someone else killed is left alone.
//   - NO SILENT ABANDONMENT: this is the run's only watcher, so a Wait error that
//     is not the daemon shutting down hands off to reconcileWatch rather than
//     returning — see the Wait-error branch. agentExecID (the id Exec returned,
//     "" for exec-less substrates) is what reconcileWatch probes for AGENT, not
//     merely container, liveness.
func (s *Server) startCompletionWatcher(runID uuid.UUID, ref, agentExecID string) {
	if s.cfg.Runner == nil {
		return
	}
	base := s.cfg.BaseCtx
	if base == nil {
		base = context.Background()
	}
	go func() {
		// Contain a panic in the detached watcher (e.g. a driver Wait bug) so it
		// can't crash the daemon; record it for forensics (mirrors reconcileWatch).
		defer func() {
			if r := recover(); r != nil {
				s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
					runID.String(), "failure", mustJSON(map[string]any{"panic": fmt.Sprintf("%v", r)})))
			}
		}()
		exitCode, werr := s.cfg.Runner.Wait(base, ref)
		if werr != nil {
			// Audit the watcher's exit for forensics either way.
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				runID.String(), "failure", mustJSON(map[string]any{"error": werr.Error()})))
			if base.Err() != nil {
				// Shutdown / daemon stopping: leave the run as-is for the next boot's
				// ReconcileOnBoot to observe. Forcing a state change here would
				// false-fail a healthy run the restart is about to re-adopt.
				return
			}
			// NOT shutdown. Driver.Wait gives up on the FIRST probe error, so one
			// docker API blip errors every in-flight Wait while the agents keep
			// working — and this is the run's ONLY watcher (one per dispatch, never
			// respawned). A transient probe error is NOT "the run finished": returning
			// here strands a RUNNING run with a live sandbox and un-revoked
			// credentials, with no other writer to finalize it. A kill, by contrast,
			// already set the terminal state, so the handoff below is a no-op for it.
			// Hand off to the watcher that already tolerates bounded probe errors and
			// then finalizes + revokes + tears down (reconcile.go).
			s.reconcileWatch(base, runID, ref, agentExecID)
			return
		}

		// Map exit code to terminal state: 0 => COMPLETED, non-zero => FAILED.
		terminal := types.RunCompleted
		outcome := "success"
		if exitCode != 0 {
			terminal = types.RunFailed
			outcome = "failure"
		}

		// KILLED-race guard: transition ONLY from RUNNING. If a kill/stop already
		// moved the run to a terminal state, applied is false and we leave it be.
		applied, uerr := s.cfg.Store.UpdateRunStateIf(base, runID, types.RunRunning, terminal)
		if uerr != nil {
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				runID.String(), "failure", mustJSON(map[string]any{
					"exit_code": exitCode, "error": uerr.Error(),
				})))
			return
		}
		if !applied {
			// Run already terminal (e.g. KILLED by a user mid-run). Do nothing —
			// the kill path already tore the sandbox down.
			return
		}

		s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
			runID.String(), outcome, mustJSON(map[string]any{
				"exit_code": exitCode, "state": terminal,
			})))

		// KILL-SWITCH CASCADE ON EVERY TERMINAL TRANSITION (matches the docs:
		// revocation fires on every run stop, "including failure"). We won the
		// RUNNING->terminal transition, so this is the authoritative end of the
		// run: deny-list the run token (Identity.RevokeRun) and revoke any minted
		// broker credentials (Broker.RevokeRun), mirroring handleKillRun. Both are
		// nil-safe and best-effort; a revocation error is audited, never fatal.
		s.revokeRunCascade(base, runID)

		// We won the transition: free the sandbox resources. Idempotent on a gone
		// sandbox. Only reached when the watcher (not a concurrent kill) advanced
		// the state, so we never tear down a run someone else killed.
		if serr := s.cfg.Runner.StopSandbox(base, ref); serr != nil {
			s.recordAudit(base, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.complete",
				ref, "failure", mustJSON(map[string]any{
					"exit_code": exitCode, "teardown_error": serr.Error(),
				})))
		}

		// Reconcile a governed workspace run that ended WITHOUT delivering its
		// result (e.g. the sandbox couldn't reach the control plane) so the
		// workspace never hangs in scanning/verifying forever.
		s.reconcileWorkspaceRun(base, runID)
		// Capture a record run's evidence from its audit events (server-side,
		// pure — no upload involved) now that the run is terminal.
		s.reconcileRecordRun(base, runID)
	}()
}

// isTerminalRunState reports whether a run is in a terminal (already-ended)
// state. The kill-switch must not re-kill or clobber a run that already ended
// (COMPLETED/FAILED by the watcher, or KILLED/STOPPED/ARCHIVED earlier).
func isTerminalRunState(st types.RunState) bool {
	switch st {
	case types.RunCompleted, types.RunFailed, types.RunKilled, types.RunStopped, types.RunArchived:
		return true
	default:
		return false
	}
}

// stopSandboxOrAudit tears a dispatch-created sandbox down and RECORDS a failed
// teardown under `action`. Silence is not an option at these sites: each one
// lands the run terminal immediately afterwards (KILLED by the racing kill, or
// FAILED via failAndRevoke), and ReconcileOnBoot skips terminal runs
// (reconcile.go), so a swallowed StopSandbox error abandons a live/routable
// container — plus its proxy sidecar, which resolved the injected credential
// VALUES into memory at startup, and RevokeRun only denies FUTURE mints —
// forever with no record. Mirrors the teardown_error audit reconcileFinalize and
// the completion watcher already emit. Reports whether the sandbox was observed
// gone, so a caller may fail closed on a teardown it cannot confirm.
func (s *Server) stopSandboxOrAudit(ctx context.Context, runID uuid.UUID, ref, action string) bool {
	if s.cfg.Runner == nil || ref == "" {
		return true
	}
	serr := s.cfg.Runner.StopSandbox(ctx, ref)
	if serr == nil {
		return true
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
		ref, "failure", mustJSON(map[string]any{
			"sandbox_ref": ref, "teardown_error": serr.Error(),
		})))
	return false
}

// revokeRunCascade performs the identity + broker revocation half of the
// kill-switch cascade for a run: it deny-lists the run token
// (Identity.RevokeRun) and revokes any minted broker credentials
// (Broker.RevokeRun). Both are nil-safe and best-effort; an error is audited as
// run.revoke/failure but never propagated (revocation must not gate the caller).
// It is shared by handleKillRun and the completion watcher so EVERY terminal
// transition revokes (matching the documented cascade-on-every-stop promise).
func (s *Server) revokeRunCascade(ctx context.Context, runID uuid.UUID) {
	data := map[string]any{}
	if s.cfg.Identity != nil {
		if rerr := s.cfg.Identity.RevokeRun(ctx, runID); rerr != nil {
			data["identity_error"] = rerr.Error()
		}
	}
	if s.cfg.Broker != nil {
		if berr := s.cfg.Broker.RevokeRun(ctx, runID); berr != nil {
			data["broker_error"] = berr.Error()
		}
	}
	if len(data) > 0 {
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.revoke",
			runID.String(), "failure", mustJSON(data)))
	}
}

// failAndRevoke transitions a run from `from` to FAILED and, if that CAS won, runs
// the credential revoke cascade — so a run that dies on ANY create/dispatch failure
// path has its minted identity + broker credentials revoked, not merely its state
// flipped. Every terminal transition must revoke (the documented cascade-on-every-
// stop promise); previously only the completion watcher, kill, and reconciler did,
// leaving the create/dispatch FAILED paths leaking a live run token + broker creds
// (C003). Revoke runs only when THIS transition won, so a concurrent kill that
// already moved the run is not double-handled.
func (s *Server) failAndRevoke(ctx context.Context, runID uuid.UUID, from types.RunState) {
	applied, err := s.cfg.Store.UpdateRunStateIf(ctx, runID, from, types.RunFailed)
	if err != nil {
		// "The compensator itself failed" is categorically different from
		// applied==false ("a concurrent kill legitimately won") and must not collapse
		// into it: nobody else will write this run's terminal state, so it strands
		// non-terminal with un-revoked credentials until the next boot reconciles it.
		log.Printf("wardynd: failAndRevoke %s: CAS %s->FAILED failed, run may be stranded: %v", runID, from, err)
		s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.fail",
			runID.String(), "failure", mustJSON(map[string]any{"from": string(from), "error": err.Error()})))
		return
	}
	if applied {
		s.revokeRunCascade(ctx, runID)
	}
}

// handleKillRun is the kill-switch: it cascades in a FIXED order — WIN the KILLED
// terminal CAS first, THEN runner teardown, identity revocation, broker credential
// revocation — then audits run.kill. The order matters: winning the CAS first means
// a kill that loses to a concurrent forward-transition 409s WITHOUT revoking, so it
// can never strip a still-live run's credentials (C002); once the transition is ours
// we tear the sandbox down (so it cannot use a credential it holds) and deny any
// future mints (identity + broker).
//
// IDEMPOTENCY / TERMINAL GUARD: a run in a NON-KILLED terminal state
// (COMPLETED/FAILED/STOPPED/ARCHIVED) is NOT re-killed — blindly writing KILLED
// would corrupt that recorded outcome — so we 409 without touching state, the
// runner, or the cascade. An already-KILLED run is the EXCEPTION (U040): its
// first kill may have failed a teardown/revoke step (the honest fail-loud path
// marks KILLED but reports the failure and advises a retry), so a re-kill must
// re-run the idempotent KillSandbox + revoke cascade to actually free the
// orphaned sandbox/credentials. Re-writing KILLED->KILLED is a value no-op.
func (s *Server) handleKillRun(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id, ok := parseIDParam(w, r, "id", "run")
	if !ok {
		return
	}
	run, err := s.cfg.Store.GetRun(ctx, id)
	if notFoundIf(w, err, "run") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get run: "+err.Error())
		return
	}

	// TERMINAL GUARD: do not clobber a NON-KILLED already-ended run. A KILLED run
	// is exempt — re-killing re-runs the idempotent teardown/revoke cascade so a
	// first kill whose teardown failed can still free the sandbox + credentials
	// (U040). COMPLETED/FAILED/STOPPED/ARCHIVED still 409 (writing KILLED would
	// corrupt the recorded outcome).
	if isTerminalRunState(run.State) && run.State != types.RunKilled {
		writeError(w, http.StatusConflict,
			"run is already terminal (state="+string(run.State)+"); not re-killing")
		return
	}

	// Run the teardown/revocation cascade and the terminal state write on a
	// context DETACHED from the request: once a kill begins it must complete even
	// if the client disconnects, or a half-applied kill could strand a live token
	// or a running sandbox (C4). Read the principal from the request first.
	killerType, killer := actorFromRequest(r)
	cascadeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()

	// (1) WIN THE TERMINAL TRANSITION FIRST (C002). Revoking before this CAS meant a
	// kill that then LOST the CAS to a concurrent dispatch forward-transition
	// (PENDING->STARTING) had already revoked the run's credentials — leaving a live
	// RUNNING run with dead creds behind a silent 409. Own the KILLED transition
	// first; only then tear down + revoke what is now unambiguously ours. Conditional
	// from the (non-terminal) state we read, so a completion watcher winning
	// RUNNING->COMPLETED is not clobbered. A re-kill of an already-KILLED run still
	// CASes KILLED->KILLED (applied), re-running the idempotent teardown (U040).
	applied, serr := s.cfg.Store.UpdateRunStateIf(cascadeCtx, id, run.State, types.RunKilled)
	if serr != nil {
		writeError(w, http.StatusInternalServerError, "update run state: "+serr.Error())
		return
	}
	if !applied {
		// The run moved to another state between our read and our write (a concurrent
		// terminal transition, or a dispatch forward-transition). Report the conflict
		// WITHOUT revoking — the run legitimately advanced, and a losing kill must not
		// strip a still-live run's credentials.
		writeError(w, http.StatusConflict,
			"run state changed concurrently; not overwriting with KILLED")
		return
	}

	killData := map[string]any{}
	// (2) Runner teardown (immediate). Idempotent on a gone sandbox.
	if s.cfg.Runner != nil && run.SandboxRef != "" {
		if kerr := s.cfg.Runner.KillSandbox(cascadeCtx, run.SandboxRef); kerr != nil {
			killData["runner_error"] = kerr.Error()
		}
	}
	// (3) Identity revocation: deny every (current+future) token for the run. A
	// bounded retry guards against a transient store error leaving the run's
	// JWT-SVID valid (until its <=1h TTL) while the kill is reported as success.
	if rerr := retryQuick(cascadeCtx, func() error { return s.cfg.Identity.RevokeRun(cascadeCtx, id) }); rerr != nil {
		killData["identity_error"] = rerr.Error()
	}
	// (4) Broker credential revocation (best-effort; audits per minted jti).
	if s.cfg.Broker != nil {
		if berr := retryQuick(cascadeCtx, func() error { return s.cfg.Broker.RevokeRun(cascadeCtx, id) }); berr != nil {
			killData["broker_error"] = berr.Error()
		}
	}

	// HONEST OUTCOME (C2): the kill-switch is the central governance control and
	// the audit log is the system of record. If ANY teardown/revocation step
	// failed, the run is marked KILLED but a minted token may still be valid until
	// its TTL or the sandbox may still be live — so we must NOT report success.
	// Emit run.kill with the TRUE outcome plus a distinct run.revoke/failure
	// event, and return 500 so the operator/CLI retries instead of believing the
	// run is contained.
	outcome := "success"
	if len(killData) > 0 {
		outcome = "failure"
	}
	s.recordAudit(cascadeCtx, s.auditEvent(&id, killerType, killer, "run.kill",
		id.String(), outcome, mustJSON(killData)))

	// A killed workspace run still settles its workspace: verify/scan runs get
	// the no-result reconcile; a record run's kill IS the normal "Done recording"
	// for interactive mode (which has no completion watcher), so capture here.
	s.reconcileWorkspaceRun(cascadeCtx, id)
	s.reconcileRecordRun(cascadeCtx, id)

	if outcome == "failure" {
		s.recordAudit(cascadeCtx, s.auditEvent(&id, types.ActorSystem, "wardynd", "run.revoke",
			id.String(), "failure", mustJSON(killData)))
		writeJSON(w, http.StatusInternalServerError, map[string]any{
			"id":     id,
			"state":  types.RunKilled,
			"errors": killData,
			"error":  "run marked KILLED but one or more teardown/revocation steps failed; the run may not be fully contained — retry the kill",
		})
		return
	}

	writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": types.RunKilled})
}

// retryQuick runs fn up to 3 times with a short linear backoff, returning the
// last error. It stops early if ctx is done. Used by the kill-switch revocation
// steps so a transient store error does not leave a token valid while the kill is
// reported as success.
func retryQuick(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return err
			case <-time.After(time.Duration(attempt) * 100 * time.Millisecond):
			}
		}
		if err = fn(); err == nil {
			return nil
		}
	}
	return err
}
