// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net"
	"slices"
	"strconv"
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
// phases of dispatchRun.
type llmTransport struct {
	// modelRun: this dispatch actually invokes the model (see the doc comment
	// on resolveLLMTransport's local modelRun below) — false for task-mode=exec
	// and for a non-interactive scan run. buildRunMounts reads this to
	// drop the resident ~/.claude mount (claudeCredTarget/claudeCredJSONTarget)
	// from a non-model run's spec even when the resolved POLICY still carries
	// it (e.g. an operator's subscription-blessed default/named policy reused
	// for a plain exec task with no per-run integration consent) — every OTHER
	// injection mode below already gates on this same signal; the mount was
	// the one path that did not.
	modelRun bool
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
	// injectBedrockSSO: Bedrock on the CAPTURED-AWS-SSO lane with Phase B on —
	// the SSO access token is injected proxy-side onto this run's own
	// portal.sso host (TLS-MITM) and the sandbox's token cache holds only a
	// placeholder. Distinct from injectBedrockBearer: a different host, a
	// different header, a different credential, and the only one of the two
	// whose credential can lapse mid-run and be recovered by a person signing
	// in (see internal/api/injection_awssso.go).
	injectBedrockSSO bool
	// secretEnvKeys are the sandboxEnv variables applyBedrockTransport filled
	// with REAL credential material — the resident SigV4 keys, or the captured
	// AWS SSO blob. Nil for every never-resident mode (bearer, ~/.aws mount) and
	// for every non-Bedrock transport, whose env holds only placeholders. Read
	// by dispatch's splitSecretEnv, which moves them onto SandboxSpec.SecretEnv
	// so a substrate does not have to publish them in a readable pod spec.
	secretEnvKeys []string
}

// isModelRun reports whether a dispatch actually invokes the model. Two run
// kinds make NO model call and so must receive NO LLM credential (least
// privilege): a scan run (execs wardyn-scan — workspaceID/sourceID set,
// non-interactive), and a task-mode=exec run — the BYOA/CI plain-command lane
// whose `wardyn run --task-mode exec` contract is literally "no agent, no LLM
// credentials". Without the exec term, a CI exec job with a connected
// managed/resident subscription plus any egress (which docs/CI.md itself
// tells operators to add) silently gets a live Anthropic OAuth token injected
// proxy-side + api.anthropic.com appended to its allow-list, for a plain
// shell command that never asked for a model. An INTERACTIVE workspace-linked
// run (Record Mode) is human-driven, not a scan, so it stays a model run.
// Mirrors the WARDYN_SCAN_ONLY discriminator. Extracted as its own function
// (rather than inlined in resolveLLMTransport) so it's independently unit
// testable — this gate gets it wrong once and every non-model run leaks a
// live model credential.
func isModelRun(taskMode string, workspaceID, sourceID *uuid.UUID, interactive bool) bool {
	return taskMode != "exec" && !((workspaceID != nil || sourceID != nil) && !interactive)
}

// resolveLLMTransport decides which LLM transport credentials this run and sets
// the corresponding sandbox env (ANTHROPIC_BASE_URL / CLAUDE_CONFIG_DIR /
// placeholders / Bedrock env / the codex-cli OpenAI gateway route). It may
// append Bedrock egress hosts to policy.AllowedDomains and register resident
// SigV4 creds with the mask registry. injections is read-only here (it gates
// the managed fallback); the grant-authoring phases mutate it later. See the
// inline comments for the full precedence rationale: host-staged mount >
// managed > Bedrock > api-key.
//
// That order is unconditional — it knows nothing about what an admin declared —
// so when an AgentProviders row names the mechanism for this run's agent, the
// transport resolved here is COMPARED against that declaration and a mismatch
// fails the run closed instead of being served by another provider's credential
// (enforceConfiguredLLMMechanism).
// sso is WHOSE captured AWS SSO session this run may use — resolved from the
// roster by the caller, because the caller is the one holding the site config.
// The zero value is the operator namespace, i.e. every deployment that never
// declared a per_user credential source.
func (s *Server) resolveLLMTransport(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, sandboxEnv map[string]string, injections []runner.InjectionGrant, interactive bool, taskMode string, proxyURL string, bedrockRef *types.WorkspaceBedrockRef, sso awsSSOScope) llmTransport {
	var t llmTransport

	// modelRun gates EVERY proxy-side credential-injection mode below
	// (subscription, managed, Bedrock) on a run that actually invokes the model.
	// See isModelRun's doc comment for the full rationale.
	modelRun := isModelRun(taskMode, run.WorkspaceID, run.SourceID, interactive)
	t.modelRun = modelRun

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
	// SubscriptionPostureOK first: on a multi-user deployment we do not author the
	// grant at all, so the run degrades to reconcileLLMAccess's honest "no model
	// access" note instead of dying at the proxy with "injection status 403" —
	// buildInjector fails closed on any non-200, and an operator reads that as a
	// Wardyn bug rather than a deliberate refusal. The sink still refuses; this
	// layer exists so the refusal is legible.
	t.injectSub = s.cfg.SubscriptionPostureOK && modelRun && t.subscription && s.cfg.SubscriptionToken != nil && !s.cfg.DisableSubscriptionInject

	// A harness login run has no credential yet — its whole purpose is for the
	// operator to run `claude setup-token` in the attach shell and mint one. Point
	// the CLI at the real API (its OAuth flow tunnels to the allowlisted OAuth
	// hosts through HTTPS_PROXY) and seed NO api-key placeholder, so nothing
	// mis-signals api-key mode. No mount, no injection, no MITM — computed BEFORE
	// the Bedrock block below so it can gate resolveBedrockAuth itself, not just
	// the sandboxEnv branch: every consumer of llm.bedrock* —
	// resident_env's ~/.aws mount (runs_dispatch.go), the bearer grant + MITM
	// host (runs_dispatch.go, dispatch.go) — reads bedrockReady/injectBedrockBearer
	// directly, so leaving t.bedrock resolved (even though sandboxEnv correctly
	// skipped applyBedrockTransport for this run) still handed a login box the
	// host's AWS credentials or a minted bearer token it never asked for and has
	// no attach-shell affordance to use.
	t.harnessLogin = run.Task == harnessLoginTask

	// Bedrock: a third Anthropic transport, mutually exclusive with subscription
	// (checked first) and api-key mode (the fallback). See resolveBedrockAuth for
	// the readiness rule and the resident-AWS-cred rationale.
	//
	// Bedrock honors the same modelRun gate computed at the top: a scan or
	// exec run signs no Bedrock request, so it gets no resident AWS SigV4 creds.
	// bedrockRef is the picked workspace/container's per-run region/model
	// override (nil => the global operator config).
	if !t.harnessLogin {
		// refresh=true: dispatch (like the real launch's create) may redeem a captured AWS SSO
		// session's rotating refresh token and persist the rotated pair.
		t.bedrock = s.resolveBedrockAuth(ctx, run.Agent, t.subscription, modelRun, true, bedrockRef, sso)
		t.bedrockReady = t.bedrock.ready
		// injectBedrockBearer wires bedrock-runtime for proxy-side bearer injection
		// (never-resident); consumed by the CA / injection / MITM-host wiring
		// alongside the subscription path.
		t.injectBedrockBearer = t.bedrockReady && t.bedrock.bearer
		// injectBedrockSSO wires the run's own portal.sso host for proxy-side
		// injection of the captured SSO access token (PHASE B). Same shape as
		// the bearer flag above, and gated on the same resolved posture: it is
		// true only when resolveBedrockAuth actually SELECTED the captured-SSO
		// lane AND the kill switch was on when it did.
		t.injectBedrockSSO = t.bedrockReady && t.bedrock.ssoInject && t.bedrock.ssoProxyInject
	}

	// MANAGED subscription: when there is no resident ~/.claude mount and no
	// Bedrock, and the operator connected a Wardyn-managed setup-token, inject it
	// PROXY-SIDE exactly like a resident subscription (the sandbox holds only an
	// inert sentinel). This is the compose-mode subscription path. Precedence:
	// host-staged mount > managed > Bedrock > api-key.
	//
	// OPT-OUT (do NOT override an explicit api-key choice): managed is the FALLBACK
	// when nothing else credentials the run — NOT a silent replacement for an
	// operator who chose api-key. An anthropic api-key grant already present in
	// `injections` (compose's ensureLLMGrant when no subscription integration
	// resolved, or a direct api-key run) means the operator opted for api-key;
	// letting managed fire would drop that grant below and silently bill the
	// subscription instead, while the compose review said "api-key". So require
	// no pre-existing anthropic injection.
	// A zero-egress policy (no allow-all, empty allow-list — e.g. a sealed demo
	// sandbox) suppresses the fallback entirely: managed injection APPENDS
	// api.anthropic.com to the allow-list below, and a fallback must not silently
	// widen a policy the operator authored as sealed. Operator-staged subscription
	// mounts are policy-blessed and unaffected.
	// Posture first, for the same reason as injectSub above — and it matters more
	// here: this lane is a DEFAULT FALLBACK for every claude-code run, needing no
	// policy, no integration id and no flag, so on a multi-user stack it silently
	// serves the operator's subscription to every member.
	managed := s.managedSubscriptionLane(run.Agent, modelRun, t.harnessLogin, t.subscription, t.bedrockReady,
		s.hasAnthropicAPIKeyInjection(run.Agent, injections), policy)
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
		t.secretEnvKeys = s.applyBedrockTransport(ctx, run, t.bedrock, policy, sandboxEnv)
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

// managedSubscriptionLane reports whether the Wardyn-managed setup-token lane
// credentials this run. It is a function rather than an expression inside
// resolveLLMTransport because CREATE resolves the same lane (resolveRunLLMLanes)
// to decide what a run WOULD dispatch on, and the two spellings drifted the
// moment there were two: create's copy was missing the posture term and the
// Bedrock term, so on any multi-user deployment — the only kind that has an
// agent roster at all — create computed "managed" for a run dispatch would
// credential some other way, and the declared-mechanism gate then refused at one
// end or the other. One spelling, both callers, no drift.
//
// Each term, in the order it matters:
//   - posture: a shared subscription is a single-user desktop setting; on a
//     multi-user stack this lane would silently serve the operator's own
//     subscription to every member.
//   - modelRun / !harnessLogin: a run that makes no model call gets no
//     credential, and the login box has none to be given yet.
//   - !subscription / !bedrockReady: managed is the FALLBACK — the host-staged
//     mount and a resolved Bedrock posture both outrank it.
//   - !apiKey: an api_key grant already brokered for this agent's provider host
//     is the operator's explicit choice; letting managed fire would drop it and
//     bill the subscription instead.
//   - managedInjectReady: a managed token is actually connected (claude-code only).
//   - some egress: managed injection APPENDS api.anthropic.com to the allow-list,
//     and a fallback must not widen a policy its author sealed.
func (s *Server) managedSubscriptionLane(agent string, modelRun, harnessLogin, subscription, bedrockReady, apiKey bool,
	policy *types.RunPolicySpec,
) bool {
	return s.cfg.SubscriptionPostureOK && modelRun && !harnessLogin && !subscription && !bedrockReady &&
		!apiKey && s.managedInjectReady(agent) &&
		(policy.AllowAllEgress || len(policy.AllowedDomains) > 0)
}

// applyBedrockTransport wires a READY Bedrock posture onto the run: it copies
// the resolved Bedrock env into the sandbox env, registers any resident SigV4
// credentials with the mask registry, appends the Bedrock egress hosts to the
// policy allow-list, and audits which of the four modes (bearer / sso-inject /
// aws-dir-mount / resident) credentials the run. Extracted verbatim from
// resolveLLMTransport's t.bedrockReady branch.
func (s *Server) applyBedrockTransport(ctx context.Context, run types.AgentRun, b bedrockAuth, policy *types.RunPolicySpec, sandboxEnv map[string]string) []string {
	for k, v := range b.env {
		sandboxEnv[k] = v
	}
	// Resident SigV4 creds must stay out of PTY/recording streams and any
	// `agent-run --selftest` echo. Bearer mode holds only a placeholder and the
	// ~/.aws-mount mode holds no keys in env at all (the SDK reads the mount), so
	// neither has anything secret to mask here.
	//
	// secretEnvKeys names the SAME variables, decided by the SAME condition and
	// in the same place, so the two answers to "which of these is a credential?"
	// cannot drift: what is worth masking out of a recording is exactly what is
	// worth keeping out of an API-readable pod spec (SandboxSpec.SecretEnv).
	// AWS_SESSION_TOKEN is conditional because a long-lived key pair has none;
	// the SSO blob is a separate mode whose env carries no SigV4 key at all, but
	// whose base64 payload IS the captured access/refresh token.
	var secretEnvKeys []string
	if !b.bearer && !b.awsMount {
		for _, k := range []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN"} {
			if b.env[k] != "" {
				secretEnvKeys = append(secretEnvKeys, k)
			}
		}
	}
	if b.ssoInject && b.env[awsSSOConfigEnvVar] != "" {
		secretEnvKeys = append(secretEnvKeys, awsSSOConfigEnvVar)
	}
	if s.cfg.MaskRegistry != nil && !b.bearer && !b.awsMount {
		s.cfg.MaskRegistry.Add(run.ID, []byte(b.env["AWS_ACCESS_KEY_ID"]))
		s.cfg.MaskRegistry.Add(run.ID, []byte(b.env["AWS_SECRET_ACCESS_KEY"]))
		if tok := b.env["AWS_SESSION_TOKEN"]; tok != "" {
			s.cfg.MaskRegistry.Add(run.ID, []byte(tok))
		}
	}
	unionAllowedDomains(policy, b.egressHosts)
	detail := "resident AWS SigV4 credentials in sandbox env (SigV4 request signing can't be proxy-injected like a static api key); IAM least-privilege scoping is the operator's responsibility"
	mode := "resident"
	switch {
	case b.bearer:
		detail = "bearer token injected proxy-side into bedrock-runtime (TLS-MITM); sandbox holds only a placeholder — never resident"
		mode = "bearer"
	case b.ssoInject && b.ssoProxyInject:
		// Phase B. The synthetic ~/.aws still exists — the SDK needs the
		// profile to know WHICH account/role to ask for — but its token cache
		// holds an inert placeholder, and the session itself is set on the wire
		// by the proxy at that one host. The ROLE credentials the SDK mints from
		// it are still resident; SigV4 signs in-process and always will.
		detail = "captured AWS SSO session injected proxy-side as x-amz-sso_bearer_token on the run's own portal.sso host (TLS-MITM); the sandbox's token cache holds only a placeholder — the SSO access token is never resident. The short-lived role credentials the SDK mints from it still are (SigV4 signs client-side)"
		mode = "sso-inject-proxy"
	case b.ssoInject:
		detail = "captured AWS SSO session materialized as a minimal synthetic ~/.aws; the sandbox SDK exchanges it for short-lived role credentials (portal.sso GetRoleCredentials). The SSO access token IS resident — Phase B (WARDYN_AWS_SSO_PROXY_INJECT) injects it proxy-side on portal.sso instead"
		mode = "sso-inject"
	case b.awsMount:
		detail = "host ~/.aws bind-mounted read-only; the AWS SDK resolves credentials (incl. auto-refreshing SSO) from the mount — no static keys stored, none resident in env"
		mode = "aws-dir-mount"
	}
	s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.llm.bedrock",
		run.ID.String(), "success", mustJSON(map[string]any{
			"region": b.region, "model": b.model, "hosts": b.egressHosts,
			// The EFFECTIVE data-plane host — a WARDYN_BEDROCK_BASE_URL
			// (PrivateLink) override's host, else the regional public one — so
			// the record names where the call actually went rather than leaving
			// an auditor to infer it from the region.
			"endpoint": b.runtimeHost,
			"mode":     mode, "detail": detail,
		})))
	return secretEnvKeys
}

// provisionDispatchMITMCA provisions the per-run TLS-MITM CA when any consumer
// needs it (subscription/managed injection, intercept_tls content inspection,
// artifact-token injection, or Bedrock bearer injection). The PRIVATE key
// reaches ONLY the proxy sidecar (ProxyConfig); the sandbox trusts the PUBLIC
// cert, installed by agent-run from WARDYN_MITM_CA_PEM and pointed at via
// NODE_EXTRA_CA_CERTS (additive for Node clients like Claude Code). On failure
// it marks the run FAILED (CAS from STARTING so a concurrent kill's KILLED
// state is preserved), audits, and returns ok=false — the dispatch must stop.
// Extracted verbatim from dispatchRun.
func (s *Server) provisionDispatchMITMCA(ctx context.Context, run types.AgentRun, sandboxEnv map[string]string) (certPEM, keyPEM string, ok bool) {
	pemCert, pemKey, caErr := generateRunCA(time.Now())
	if caErr != nil {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "could not provision the per-run TLS-interception CA: "+caErr.Error())
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "mitm ca: " + caErr.Error()})))
		return "", "", false
	}
	certPEM, keyPEM = string(pemCert), string(pemKey)
	sandboxEnv["WARDYN_MITM_CA_PEM"] = certPEM
	// CA files live under /tmp/wardyn (any-uid-writable, works for images with
	// arbitrary USER/HOME — the old /home/agent/.wardyn pin dangled for
	// envbuilder/BYOI images whose HOME differs; precedent: mainProcCastDir).
	// WHICH variables, and why their values differ, is sandboxCATrustVars'
	// business — this run's own CA is authoritative here, so nothing is spared.
	setSandboxCATrustVars(sandboxEnv, false)
	return certPEM, keyPEM, true
}

// sandboxCATrustVars is THE list of CA-trust variables a sandbox's toolchains
// read, and the single place any of them is named. It exists because this list
// was duplicated across the two callers here and drifted: AWS_CA_BUNDLE was
// added to one and missed on the other, which left the sandbox's AWS CLI
// distrusting Wardyn's OWN per-run interception CA on the Bedrock lane the
// product drives itself. One list, two callers, nothing to keep in sync.
//
// "Install the CA" is one action PER TOOLCHAIN, and the values are not
// interchangeable. NODE_EXTRA_CA_CERTS is ADDITIVE (Node keeps its bundled
// roots) so it takes the BARE CA. Every other variable REPLACES its trust
// store outright, so each takes the COMBINED bundle (system roots + the
// per-run CA) that install_mitm_ca/agentIdleScript assemble — the bare CA
// there would break verification of every non-MITM'd CONNECT-tunneled host.
// AWS CLI v2 needs its own entry because it ships its own Python and its own
// CA store and reads none of the other four. The JVM keystore and Deno's
// DENO_CERT are covered by no variable at all and are documented as not
// covered (deploy/images/README.md).
func sandboxCATrustVars() map[string]string {
	return map[string]string{
		"NODE_EXTRA_CA_CERTS": "/tmp/wardyn/mitm-ca.pem",
		"SSL_CERT_FILE":       "/tmp/wardyn/ca-bundle.pem",
		"REQUESTS_CA_BUNDLE":  "/tmp/wardyn/ca-bundle.pem",
		"CURL_CA_BUNDLE":      "/tmp/wardyn/ca-bundle.pem",
		"AWS_CA_BUNDLE":       "/tmp/wardyn/ca-bundle.pem",
	}
}

// setSandboxCATrustVars writes that list into sandboxEnv. onlyIfUnset leaves
// any value already staged alone, which is what the corporate-CA append needs:
// a MITM'd run's own /tmp/wardyn paths must never be clobbered.
func setSandboxCATrustVars(sandboxEnv map[string]string, onlyIfUnset bool) {
	for k, v := range sandboxCATrustVars() {
		if onlyIfUnset && sandboxEnv[k] != "" {
			continue
		}
		sandboxEnv[k] = v
	}
}

// installSandboxTrustedCA appends the operator's corporate PEM
// (WARDYN_TRUSTED_CA_FILE, corpPEM) to this run's sandbox CA trust, so a
// TLS-inspecting corporate middlebox on the path between wardyn-proxy and the
// real internet is trusted by the sandbox's OWN TLS clients on a passthrough
// (non-MITM'd) CONNECT tunnel — not only by wardynd and wardyn-proxy
// themselves (see trusted_ca.go / proxy/server.go). corpPEM == "" (the knob
// unset) is a no-op: sandboxEnv is left exactly as provisionDispatchMITMCA
// (or nothing, on a non-MITM run) left it.
//
// install_mitm_ca (agent-run-lib.sh) treats WARDYN_MITM_CA_PEM as an opaque
// PEM bundle already — multiple certificates just concatenate — so appending
// here is additive to whatever provisionDispatchMITMCA staged for this run's
// OWN per-run interception CA. On a run with NO Wardyn-side MITM (that block
// above never ran), WARDYN_MITM_CA_PEM would otherwise be entirely absent and
// install_mitm_ca would no-op, so this also SEEDS it plus the five bundle
// vars the sandbox's toolchains need — but only when provisionDispatchMITMCA
// has not already set them, so a MITM'd run's own /tmp/wardyn paths are never
// touched here.
//
// Five, not four: "install the CA" is one action PER TOOLCHAIN, and AWS CLI v2
// ships its OWN Python and its OWN CA store — it reads none of the other four.
// Without AWS_CA_BUNDLE a MITM'd Bedrock/STS call still fails, in a lane the
// product drives itself. deploy/images/README.md carries the per-toolchain
// table; the JVM keystore is addressed by no variable at all.
func installSandboxTrustedCA(corpPEM string, sandboxEnv map[string]string) {
	if corpPEM == "" {
		return
	}
	if existing := sandboxEnv["WARDYN_MITM_CA_PEM"]; existing != "" {
		sandboxEnv["WARDYN_MITM_CA_PEM"] = existing + "\n" + corpPEM
	} else {
		sandboxEnv["WARDYN_MITM_CA_PEM"] = corpPEM
	}
	setSandboxCATrustVars(sandboxEnv, true)
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
// dispatchRun.
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
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "could not author the "+injectSource+" credential injection: "+gerr.Error())
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": injectSource + " inject grant: " + gerr.Error()})))
		return injections, false
	}
	if rule, derr := injectionRuleFromScope(subScope); derr == nil {
		injections = append(injections, runner.InjectionGrant{GrantID: subGrantID, Rule: rule})
	}
	unionAllowedDomains(policy, []string{anthropicAPIHost})
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
// dispatchRun.
//
// The MITM entry is "host:port" (net.JoinHostPort), NOT a bare host, for the
// same reason planArtifactRedirect authors one (artifact_redirect.go): a bare
// entry is ANY-PORT in the proxy (parseMITMHostPort returns port 0, and
// handleConnect's `cport == 0 || cport == port` then matches everything), so
// an agent that can reach the Bedrock host at all could CONNECT to it on a
// port nobody configured and have that tunnel TLS-terminated with the Wardyn
// leaf and the operator's Bearer injected onto whatever answered there.
// The injection SCOPE below stays a bare host — buildInjector requires that —
// only the MITM-eligibility set carries the port.
//
// The scope's snapshot records WHOSE key the resolve read (bedrockAuth's
// bearerNamespace): the sink resolves the key from exactly that namespace and
// refuses a grant without one — see resolveBedrockBearerInjection.
func (s *Server) authorBedrockBearerInjection(ctx context.Context, run types.AgentRun, t llmTransport, injections []runner.InjectionGrant) ([]runner.InjectionGrant, []string, bool) {
	mitmHosts := []string{net.JoinHostPort(t.bedrock.runtimeHost, strconv.Itoa(t.bedrock.runtimePort))}
	beScope, _ := json.Marshal(map[string]any{
		"host":        t.bedrock.runtimeHost,
		"header":      "Authorization",
		"format":      "Bearer %s",
		"secret_name": bedrockAPIKeySecret,
		"snapshot":    bedrockBearerSnapshotOf(t.bedrock.bearerNamespace),
	})
	beGrantID := uuid.New()
	if _, gerr := s.cfg.Store.CreateGrant(ctx, types.CredentialGrant{
		ID: beGrantID, RunID: run.ID, CreatedAt: time.Now(),
		Spec: types.GrantSpec{Kind: types.GrantAPIKey, Scope: beScope, TTLSeconds: 3600},
	}); gerr != nil {
		// CAS from STARTING (claimed at dispatch entry) so a concurrent kill's
		// KILLED state is preserved rather than clobbered back to FAILED.
		s.failAndRevoke(ctx, run.ID, types.RunStarting, "could not author the Bedrock bearer credential injection: "+gerr.Error())
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": "bedrock bearer inject grant: " + gerr.Error()})))
		return injections, nil, false
	}
	if rule, derr := injectionRuleFromScope(beScope); derr == nil {
		injections = append(injections, runner.InjectionGrant{GrantID: beGrantID, Rule: rule})
	}
	return injections, mitmHosts, true
}

// dropUnauthoredBedrockBearerInjections removes every injection naming
// bedrock-api-key, auditing each one.
//
// The key has ONE author: authorBedrockBearerInjection, whose grant records the
// namespace the key was read from. Any other injection naming it — a stored
// policy's, a recorded profile that captured an earlier run's grant — carries
// no record of THIS run's choice, and the sink refuses it, which would fail the
// proxy's startup. Every drop is audited, as filterMemberGrants' and
// persistRunGrants' are, so an operator whose policy named the key can see why
// that injection is gone.
func (s *Server) dropUnauthoredBedrockBearerInjections(ctx context.Context, run types.AgentRun, injections []runner.InjectionGrant) []runner.InjectionGrant {
	return slices.DeleteFunc(injections, func(ig runner.InjectionGrant) bool {
		if ig.Rule.SecretName != bedrockAPIKeySecret {
			return false
		}
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.injection.dropped",
			ig.GrantID.String(), "denied", mustJSON(map[string]any{
				"grant_id": ig.GrantID, "secret_name": bedrockAPIKeySecret, "host": ig.Rule.Host,
				"reason": "bedrock_bearer_not_dispatch_authored",
			})))
		return true
	})
}

// llmInspectMITMEnabled reports whether the policy's intercept_tls content
// inspection is active (mode set and not "off") — the content-inspection reason
// to provision the per-run MITM CA and TLS-terminate the built-in LLM hosts.
func llmInspectMITMEnabled(policy *types.RunPolicySpec) bool {
	li := policy.LLMInspection
	return li != nil && li.InterceptTLS && li.Mode != "" && !strings.EqualFold(li.Mode, "off")
}

// inspectableLLMRefusal is require_inspectable_llm's refusal. It
// enumerates BOTH Bedrock sub-modes: the gate below fails the bearer lane closed
// too, and a sentence that named only SigV4 told an operator their bearer run
// was refused for a reason that did not apply to it. It is a run failure hint
// AND the quoted `error` value on the run.create failure row.
//
// DRAFT (M2 canon pending)
const inspectableLLMRefusal = "require_inspectable_llm: the resolved LLM transport is opaque (subscription " +
	"without MITM, or Bedrock — both SigV4 and bearer, which Wardyn can decrypt but has no extractor " +
	"for); enable intercept_tls or use an inspectable transport"

// enforceInspectableLLM fails CLOSED at schedule time when inspection is
// REQUIRED but the resolved LLM transport is OPAQUE. Opaque transports:
// (a) a subscription/OAuth transport that is NOT being MITM'd (injectSub /
// intercept_tls auto-enable MITM, making it inspectable); (b) BEDROCK, BOTH
// sub-modes — proxy-injected + MITM'd does NOT make Bedrock inspectable:
// require_inspectable_llm is a
// RUNTIME guarantee (policy.go), and MITM only makes a body READABLE — SCANNING
// it needs an extractor and a prompt-bearing channel, and there is neither for
// Bedrock. contentscan.Extract handles anthropic.messages / openai.chat /
// generic / mcp.jsonrpc only, and channelForHost gives a bedrock-runtime host
// ChannelGeneric, which classifyLLM treats as not prompt-bearing — so a bearer
// Bedrock run admitted as "inspectable" would get ZERO scan coverage. Both sub-modes
// therefore fail closed until a Bedrock extractor + channel exist; when they do,
// re-exempt the bearer arm HERE (one predicate) and say so in THREAT-MODEL 5.1a.
// The default (require_inspectable_llm=false) instead degrades visibly rather
// than failing. Returns false when the run was marked FAILED (CAS from STARTING
// so a concurrent kill's KILLED is not clobbered) and dispatch must stop.
func (s *Server) enforceInspectableLLM(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, llm llmTransport) bool {
	li := policy.LLMInspection
	if li == nil || !li.RequireInspectableLLM || li.Mode == "" || strings.EqualFold(li.Mode, "off") {
		return true
	}
	if (llm.subscription && !li.InterceptTLS && !llm.injectSub) || llm.bedrockReady {
		s.failAndRevoke(ctx, run.ID, types.RunStarting, inspectableLLMRefusal)
		s.recordAudit(ctx, s.auditEvent(&run.ID, types.ActorSystem, "wardynd", "run.create",
			run.ID.String(), "failure", mustJSON(map[string]any{"error": inspectableLLMRefusal})))
		return false
	}
	return true
}

// dispatchLLMPlan is what the LLM/credential-injection phase decides: the
// resolved transport, the injection list it authored, and the TLS-MITM material
// the proxy sidecar needs. A struct because the phase produces six values and
// threading six returns through dispatchRun is how one of them gets dropped.
type dispatchLLMPlan struct {
	llm        llmTransport
	injections []runner.InjectionGrant
	// mitmCACertPEM/mitmCAKeyPEM are the per-run CA; empty unless some consumer
	// needs one. The PRIVATE key reaches ONLY the proxy sidecar.
	mitmCACertPEM string
	mitmCAKeyPEM  string
	// bedrockMITMHosts is the Bedrock bearer's per-run MITM host, if any, as
	// "host:port" (never a bare host — see authorBedrockBearerInjection).
	bedrockMITMHosts []string
	// llmUnavailableDetail is the self-explaining detail the proxy's brokered-LLM
	// 404 renders when this run reaches that route with no credential behind it
	// (see llmUnavailableDetail). Empty => the route's own generic detail.
	llmUnavailableDetail string
	// mitmLLM is whether the BUILT-IN LLM hosts should be intercepted —
	// subscription/managed injection or intercept_tls inspection, never a CA
	// minted purely for artifact tokens. Computed here because every input to it
	// is decided here.
	mitmLLM bool
}

// resolveLLMInjections resolves the LLM transport and authors every proxy-side
// injection that follows from it, as ONE phase.
//
// It is a phase and not a slice taken to satisfy a complexity counter: the
// transport decision (host-staged subscription > managed > Bedrock > api-key
// gateway) is what determines whether this run needs a MITM CA, a subscription
// sentinel grant, a Bedrock bearer, or none of them — so the authoring steps are
// consequences of one decision rather than neighbours that happen to be
// adjacent. The corporate-CA install rides along because its placement is
// load-bearing: before resolveEnvSecretGrants, so a user env_secret named
// SSL_CERT_FILE cannot clobber the bundle it stages.
//
// Fail closed, and the ok return is the whole contract: each authoring helper
// has already marked the run FAILED before returning false, so a false here
// means "stop dispatching, the run is already terminal" — exactly what the four
// bare returns meant when this lived inline.
//
// policy and sandboxEnv are MUTATED (egress widening for Bedrock, the auth env,
// the trust store); injections is returned rather than mutated because
// authorSubscriptionInjection reslices it.
func (s *Server) resolveLLMInjections(ctx context.Context, run types.AgentRun, p dispatchParams,
	policy *types.RunPolicySpec, sandboxEnv map[string]string, injections []runner.InjectionGrant,
	proxyURL string, artifactPlan artifactRedirectPlan, artifactInject bool, siteCfg types.SiteConfig, siteCfgOK bool,
) (dispatchLLMPlan, bool) {
	// WHOSE credential, decided from a roster we could actually READ. A failed
	// read yields a zero siteCfg — perUser=false, owner="" — which is the
	// OPERATOR namespace, so a store blip credentialed a per_user member's run
	// with the deployment-wide session.
	if !s.enforceReadableRosterForCredential(ctx, run, p, policy, siteCfgOK) {
		return dispatchLLMPlan{}, false
	}
	// WHOSE model credential this run may use, from the roster this phase was
	// already handed. runIdentitySubject(run.CreatedBy) is the SUBJECT the run's
	// identity was minted with — the same string every other credential-bearing
	// path resolves a namespace against — and the request's context values
	// survive dispatch's WithoutCancel, so a detached dispatch resolves the same
	// namespace the create door did.
	sso := awsSSOScopeFor(siteCfg, run.Agent, runIdentitySubject(ctx, run.CreatedBy))
	llm := s.resolveLLMTransport(ctx, run, policy, sandboxEnv, injections, p.Interactive, p.TaskMode, proxyURL, p.BedrockRef, sso)
	if p.ResolvedManaged != nil {
		*p.ResolvedManaged = llm.injectManaged
	}

	// No cross-mechanism fallback: refuse before a single credential is authored
	// when the org declared how this agent reaches its model and the transport
	// just resolved is not that one. Placed here, ahead of the MITM CA and every
	// grant author, so a refused run mints nothing — see
	// enforceConfiguredLLMMechanism. A zero-value siteCfg (the read failed) is
	// legacy open mode: nothing is refused.
	if !s.enforceConfiguredLLMMechanism(ctx, run, siteCfg, llm, injections) {
		return dispatchLLMPlan{}, false
	}

	// Optional TLS-MITM of opaque LLM CONNECT tunnels: provision a per-run CA
	// when ANY consumer needs one — intercept_tls content inspection,
	// subscription/managed credential injection, artifact-token injection, or
	// Bedrock bearer injection. The PRIVATE key reaches ONLY the proxy sidecar
	// (ProxyConfig below); the sandbox trusts the PUBLIC cert. See
	// provisionDispatchMITMCA for the trust-store wiring.
	mitmForInspect := llmInspectMITMEnabled(policy)
	var mitmCACertPEM, mitmCAKeyPEM string
	if llm.injectSub || llm.injectManaged || mitmForInspect || artifactInject || llm.injectBedrockBearer || llm.injectBedrockSSO {
		var ok bool
		if mitmCACertPEM, mitmCAKeyPEM, ok = s.provisionDispatchMITMCA(ctx, run, sandboxEnv); !ok {
			return dispatchLLMPlan{}, false
		}
	}

	// Corporate CA trust (WARDYN_TRUSTED_CA_FILE): append the operator's PEM to
	// this run's sandbox CA trust exactly as provisionDispatchMITMCA does for
	// the per-run MITM cert — see installSandboxTrustedCA. Runs unconditionally
	// (no-op when the knob is unset) so a non-MITM run gets it too. Placed
	// before resolveEnvSecretGrants below, so a user env_secret named
	// SSL_CERT_FILE can never clobber the bundle this just staged.
	installSandboxTrustedCA(s.cfg.TrustedCAPEM, sandboxEnv)

	// Subscription / managed: author the proxy-side sentinel credential grant
	// (see authorSubscriptionInjection for the re-mint + api-key-replacement
	// rationale). A failed grant write already marked the run FAILED — stop.
	if llm.injectSub || llm.injectManaged {
		var ok bool
		if injections, ok = s.authorSubscriptionInjection(ctx, run, llm, policy, injections); !ok {
			return dispatchLLMPlan{}, false
		}
	}

	// The run's own bearer, if it has one, is authored below — the only
	// injection naming bedrock-api-key the proxy is handed.
	injections = s.dropUnauthoredBedrockBearerInjections(ctx, run, injections)

	// Bedrock BEARER injection + its per-run MITM host (see
	// authorBedrockBearerInjection). Same stop-on-failure contract.
	var bedrockMITMHosts []string
	if llm.injectBedrockBearer {
		var ok bool
		if injections, bedrockMITMHosts, ok = s.authorBedrockBearerInjection(ctx, run, llm, injections); !ok {
			return dispatchLLMPlan{}, false
		}
	}

	// Captured-AWS-SSO injection + its per-run MITM host (PHASE B, see
	// authorBedrockSSOInjection). Same stop-on-failure contract as the bearer
	// block above, and it appends to the SAME MITM host list: a run is on one
	// Bedrock lane or the other, never both, so the two never collide.
	if llm.injectBedrockSSO {
		var ok bool
		var ssoMITMHosts []string
		if injections, ssoMITMHosts, ok = s.authorBedrockSSOInjection(ctx, run, llm, sso, injections); !ok {
			return dispatchLLMPlan{}, false
		}
		bedrockMITMHosts = append(bedrockMITMHosts, ssoMITMHosts...)
	}

	// Artifact-redirect token injections (authored in planArtifactRedirect, whose
	// egress substitution already added each corp host to policy.AllowedDomains, so
	// the injector's exact-allowlist check passes). Appended AFTER the subscription
	// block, which reslices `injections` in place.
	injections = append(injections, artifactPlan.injections...)

	// Fail CLOSED at schedule time when inspection is REQUIRED but the resolved
	// LLM transport is OPAQUE — see enforceInspectableLLM.
	if !s.enforceInspectableLLM(ctx, run, policy, llm) {
		return dispatchLLMPlan{}, false
	}

	return dispatchLLMPlan{
		llm: llm, injections: injections,
		mitmCACertPEM: mitmCACertPEM, mitmCAKeyPEM: mitmCAKeyPEM,
		bedrockMITMHosts:     bedrockMITMHosts,
		llmUnavailableDetail: s.llmUnavailableDetail(ctx, run, llm, injections, sso),
		mitmLLM:              llm.injectSub || llm.injectManaged || mitmForInspect,
	}, true
}
