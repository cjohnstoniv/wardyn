// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// llmTransport is the resolved LLM credential transport for one dispatch:
// which of the mutually-exclusive Anthropic paths (host-staged subscription,
// Wardyn-managed subscription, Bedrock, api-key gateway) credentials the run,
// and which proxy-side injections/MITM that implies. Produced by
// resolveLLMTransport, consumed by the CA / grant-authoring / SandboxSpec
// phases of dispatchWithVerify.
type llmTransport struct {
	// subscription: the policy bind-mounts the resident ~/.claude (claudeCredTarget).
	subscription bool
	// injectSub: subscription AND a live token provider is wired AND the
	// WARDYN_SUBSCRIPTION_INJECT escape hatch is not off — the proxy swaps in the
	// LIVE OAuth token (TLS-MITM of api.anthropic.com).
	injectSub bool
	// injectManaged: no resident mount / no Bedrock; the Wardyn-managed
	// setup-token credentials the run proxy-side (the compose-mode path).
	injectManaged bool
	// harnessLogin: a `claude setup-token` login box — no credential at all.
	harnessLogin bool
	// bedrock is the resolved Bedrock auth posture; ready gates all Bedrock use.
	bedrock      bedrockAuth
	bedrockReady bool
	// injectBedrockBearer: Bedrock in BEARER mode — proxy-side token injection
	// into bedrock-runtime (never-resident), the only inspectable Bedrock path.
	injectBedrockBearer bool
}

// resolveLLMTransport decides which LLM transport credentials this run and sets
// the corresponding sandbox env (ANTHROPIC_BASE_URL / CLAUDE_CONFIG_DIR /
// placeholders / Bedrock env / the codex-cli OpenAI gateway route). It may
// append Bedrock egress hosts to policy.AllowedDomains and register resident
// SigV4 creds with the mask registry. injections is read-only here (it gates
// the managed fallback); the grant-authoring phases mutate it later. Extracted
// verbatim from dispatchWithVerify — see the inline comments for the full
// precedence rationale: host-staged mount > managed > Bedrock > api-key.
func (s *Server) resolveLLMTransport(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, sandboxEnv map[string]string, injections []runner.InjectionGrant, verifyPlan json.RawMessage, interactive bool, proxyURL string) llmTransport {
	var t llmTransport

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
	t.subscription = specHasMountTarget(policy, claudeCredTarget)
	// Subscription runs: inject the operator's LIVE OAuth token PROXY-SIDE (the
	// sandbox holds only an inert sentinel) instead of the resident copy, which
	// goes stale — the access token expires (~hours) and the refresh token ROTATES
	// as the operator's own host `claude` refreshes, locking the copy out. This
	// REQUIRES TLS-MITM of api.anthropic.com so the proxy can swap the credential;
	// it is the safe default whenever a token provider is wired. Escape hatch:
	// WARDYN_SUBSCRIPTION_INJECT=off keeps the legacy resident-copy behavior.
	t.injectSub = t.subscription && s.cfg.SubscriptionToken != nil && !s.cfg.DisableSubscriptionInject

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
	t.bedrock = s.resolveBedrockAuth(ctx, run.Agent, t.subscription, modelRun)
	t.bedrockReady = t.bedrock.ready
	// injectBedrockBearer wires bedrock-runtime for proxy-side bearer injection
	// (never-resident); consumed by the CA / injection / MITM-host wiring
	// alongside the subscription path.
	t.injectBedrockBearer = t.bedrockReady && t.bedrock.bearer

	// A HARNESS LOGIN run has no credential yet — its whole purpose is for the
	// operator to run `claude setup-token` in the attach shell and mint one. Point
	// the CLI at the real API (its OAuth flow tunnels to the allowlisted OAuth
	// hosts through HTTPS_PROXY) and seed NO api-key placeholder, so nothing
	// mis-signals api-key mode. No mount, no injection, no MITM.
	t.harnessLogin = run.Task == harnessLoginTask

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
	managed := !t.harnessLogin && !t.subscription && !t.bedrockReady &&
		!hasAnthropicAPIKeyInjection(injections) && s.managedInjectReady(run.Agent) &&
		(policy.AllowAllEgress || len(policy.AllowedDomains) > 0)
	t.injectManaged = managed

	if t.harnessLogin {
		sandboxEnv["ANTHROPIC_BASE_URL"] = "https://api.anthropic.com"
	} else if t.subscription {
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
	} else if t.bedrockReady {
		for k, v := range t.bedrock.env {
			sandboxEnv[k] = v
		}
		// Resident SigV4 creds must stay out of PTY/recording streams and any
		// `agent-run --selftest` echo. Bearer mode holds only a placeholder and the
		// ~/.aws-mount mode holds no keys in env at all (the SDK reads the mount), so
		// neither has anything secret to mask here.
		if s.cfg.MaskRegistry != nil && !t.bedrock.bearer && !t.bedrock.awsMount {
			s.cfg.MaskRegistry.Add(run.ID, []byte(t.bedrock.env["AWS_ACCESS_KEY_ID"]))
			s.cfg.MaskRegistry.Add(run.ID, []byte(t.bedrock.env["AWS_SECRET_ACCESS_KEY"]))
			if tok := t.bedrock.env["AWS_SESSION_TOKEN"]; tok != "" {
				s.cfg.MaskRegistry.Add(run.ID, []byte(tok))
			}
		}
		for _, h := range t.bedrock.egressHosts {
			if !domainAllowedExact(policy.AllowedDomains, h) {
				policy.AllowedDomains = append(policy.AllowedDomains, h)
			}
		}
		detail := "resident AWS SigV4 credentials in sandbox env (SigV4 request signing can't be proxy-injected like a static api key); IAM least-privilege scoping is the operator's responsibility"
		mode := "resident"
		switch {
		case t.bedrock.bearer:
			detail = "bearer token injected proxy-side into bedrock-runtime (TLS-MITM); sandbox holds only a placeholder — never resident"
			mode = "bearer"
		case t.bedrock.awsMount:
			detail = "host ~/.aws bind-mounted read-only; the AWS SDK resolves credentials (incl. auto-refreshing SSO) from the mount — no static keys stored, none resident in env"
			mode = "aws-dir-mount"
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm.bedrock",
			run.ID.String(), "success", mustJSON(map[string]any{
				"region": s.cfg.BedrockRegion, "model": s.cfg.BedrockModel, "hosts": t.bedrock.egressHosts,
				"mode": mode, "detail": detail,
			})))
	} else {
		sandboxEnv["ANTHROPIC_API_KEY"] = "wardyn-proxy-injected"
	}
	// Operator model pin: force a specific Anthropic model (e.g. "opus") so the
	// agent doesn't fall back to the account/CLI default (a promo can push that to
	// a cheaper model like Fable). Off unless configured; Claude agent only; never
	// overrides the Bedrock model id (that IS the pin, in inference-profile form).
	if s.cfg.AgentAnthropicModel != "" && run.Agent == "claude-code" && !t.bedrockReady {
		sandboxEnv["ANTHROPIC_MODEL"] = s.cfg.AgentAnthropicModel
	}

	// Codex (OpenAI) reverse-proxy route: point the OpenAI SDK at the proxy's
	// inspectable /wardyn/llm/openai gateway with a non-secret placeholder; the
	// proxy strips it and injects the brokered OpenAI key (mirrors Anthropic
	// api-key mode). A subscription Codex reaching api.openai.com directly is
	// covered by TLS-MITM when intercept_tls is enabled.
	if run.Agent == "codex-cli" && !t.subscription {
		sandboxEnv["OPENAI_BASE_URL"] = proxyURL + "/wardyn/llm/openai"
		sandboxEnv["OPENAI_API_KEY"] = "wardyn-proxy-injected"
	}

	return t
}

// provisionDispatchMITMCA provisions the per-run TLS-MITM CA when any consumer
// needs it (subscription/managed injection, intercept_tls content inspection,
// artifact-token injection, or Bedrock bearer injection). The PRIVATE key
// reaches ONLY the proxy sidecar (ProxyConfig); the sandbox trusts the PUBLIC
// cert, installed by agent-run from WARDYN_MITM_CA_PEM and pointed at via
// NODE_EXTRA_CA_CERTS (additive for Node clients like Claude Code). On failure
// it marks the run FAILED (CAS from STARTING so a concurrent kill's KILLED
// state is preserved), audits, and returns ok=false — the dispatch must stop.
// Extracted verbatim from dispatchWithVerify.
func (s *Server) provisionDispatchMITMCA(ctx context.Context, run types.AgentRun, sandboxEnv map[string]string) (certPEM, keyPEM string, ok bool) {
	pemCert, pemKey, caErr := generateRunCA(time.Now())
	if caErr != nil {
		s.failAndRevoke(ctx, run.ID, types.RunStarting)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "mitm ca: " + caErr.Error()})))
		return "", "", false
	}
	certPEM, keyPEM = string(pemCert), string(pemKey)
	sandboxEnv["WARDYN_MITM_CA_PEM"] = certPEM
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
	return certPEM, keyPEM, true
}

// authorSubscriptionInjection authors the subscription/managed proxy-side
// credential: a re-mintable api_key grant whose SENTINEL secret name resolves
// to a LIVE Anthropic OAuth token (resident host token, or the Wardyn-managed
// captured setup-token) rather than a stored secret; appends its injection and
// ensures the exact host is egress-allowed (the injector's hard requirement).
// Non-approval api_key grants are re-mintable by design, so the proxy
// re-resolves the token indefinitely — this is what makes the sandbox's
// sentinel sufficient. injectSub and injectManaged are mutually exclusive by
// construction (managed requires !subscription). Returns the updated injections
// slice; ok=false means the grant write failed, the run was marked FAILED
// (CAS from STARTING), and dispatch must stop. Extracted verbatim from
// dispatchWithVerify.
func (s *Server) authorSubscriptionInjection(ctx context.Context, run types.AgentRun, t llmTransport, policy *types.RunPolicySpec, injections []runner.InjectionGrant) ([]runner.InjectionGrant, bool) {
	const anthropicAPIHost = "api.anthropic.com"
	sentinelName := subscriptionOAuthSecret
	injectSource := "subscription"
	detail := "live subscription OAuth token injected proxy-side; sandbox's staged copy holds only inert sentinel tokens (access + refresh both replaced at staging)"
	if t.injectManaged {
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
		return injections, false
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
	return injections, true
}

// authorBedrockBearerInjection authors the Bedrock BEARER injection: an api_key
// grant whose Authorization: Bearer header injects the operator's Bedrock API
// key into bedrock-runtime, and marks that host TLS-MITM-eligible for THIS run.
// This is the same operator-configured MITM-host + paired-injection pattern as
// corp artifact hosts (isCorpMITMHost) — bedrock-runtime is not a wildcard, the
// token is the operator's own, and the CA key stays in proxy memory. The
// sandbox holds only the placeholder bearer. Returns the updated injections and
// the MITM host list; ok=false means the grant write failed, the run was marked
// FAILED (CAS from STARTING), and dispatch must stop. Extracted verbatim from
// dispatchWithVerify.
func (s *Server) authorBedrockBearerInjection(ctx context.Context, run types.AgentRun, t llmTransport, injections []runner.InjectionGrant) ([]runner.InjectionGrant, []string, bool) {
	mitmHosts := []string{t.bedrock.runtimeHost}
	beScope, _ := json.Marshal(map[string]string{
		"host":        t.bedrock.runtimeHost,
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
		return injections, nil, false
	}
	if rule, derr := injectionRuleFromScope(beScope); derr == nil {
		injections = append(injections, runner.InjectionGrant{GrantID: beGrantID, Rule: rule})
	}
	return injections, mitmHosts, true
}

// llmInspectMITMEnabled reports whether the policy's intercept_tls content
// inspection is active (mode set and not "off") — the content-inspection reason
// to provision the per-run MITM CA and TLS-terminate the built-in LLM hosts.
func llmInspectMITMEnabled(policy *types.RunPolicySpec) bool {
	li := policy.LLMInspection
	return li != nil && li.InterceptTLS && li.Mode != "" && !strings.EqualFold(li.Mode, "off")
}

// enforceInspectableLLM fails CLOSED at schedule time when inspection is
// REQUIRED but the resolved LLM transport is OPAQUE (M24). Opaque transports:
// (a) a subscription/OAuth transport that is NOT being MITM'd (injectSub /
// intercept_tls auto-enable MITM, making it inspectable); (b) SigV4 Bedrock via
// ~/.aws or resident keys — uninspectable by construction (we cannot re-sign a
// MITM'd SigV4 request), so only the Bedrock BEARER path (proxy-injected,
// MITM'd) is inspectable. Previously only the subscription case failed closed,
// silently exempting opaque Bedrock. The default (require_inspectable_llm=false)
// instead degrades visibly rather than failing. Returns false when the run was
// marked FAILED (CAS from STARTING so a concurrent kill's KILLED is not
// clobbered) and dispatch must stop. Extracted verbatim from dispatchWithVerify.
func (s *Server) enforceInspectableLLM(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, llm llmTransport) bool {
	li := policy.LLMInspection
	if li == nil || !li.RequireInspectableLLM || li.Mode == "" || strings.EqualFold(li.Mode, "off") {
		return true
	}
	if (llm.subscription && !li.InterceptTLS && !llm.injectSub) || (llm.bedrockReady && !llm.bedrock.bearer) {
		s.failAndRevoke(ctx, run.ID, types.RunStarting)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "require_inspectable_llm: the resolved LLM transport is opaque (subscription without MITM, or SigV4 Bedrock); enable intercept_tls or use an inspectable transport"})))
		return false
	}
	return true
}
