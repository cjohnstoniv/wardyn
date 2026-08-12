// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GitHub App credential secret names (mirrors cmd/wardynd's secretGitHubAppID /
// secretGitHubAppKey). Presence of BOTH sets Secrets.GitHubApp.
const (
	secretGitHubAppID  = "github-app-id"
	secretGitHubAppKey = "github-app-key"
)

// First-run setup readiness surface.
//
// GET /api/v1/setup/status returns the aggregate a first-run "Getting started"
// wizard needs to detect the environment, providers, credentials, and runner
// capability of THIS control plane. The struct types below are the SINGLE
// FROZEN CONTRACT shared with the UI (ui/src/app/lib/types.ts SetupStatus) —
// keep the two in exact sync (snake_case wire fields).

// SetupStatus is the aggregate readiness snapshot for GET /api/v1/setup/status.
type SetupStatus struct {
	// Ready is server-computed and CONSERVATIVE (false when the runner is nil),
	// so the wizard opens rather than hiding a half-configured bootstrap.
	Ready bool `json:"ready"`
	// Checks is the single list of environment/readiness rows the UI renders.
	Checks []SetupCheck `json:"checks"`
	// Auth is the active public-API auth posture.
	Auth SetupAuth `json:"auth"`
	// Runner is the sandbox runner + the confinement classes actually live on
	// this host (from Runner.Capabilities, same source as /healthz).
	Runner SetupRunner `json:"runner"`
	// Composer is the AI Run Composer enablement + per-backend readiness snapshot.
	Composer SetupComposer `json:"composer"`
	// Providers reports resident coding-agent CLIs detected on the wardynd host.
	Providers []SetupProvider `json:"providers"`
	// Secrets reports which known secrets are present (NAMES only, reserved
	// names excluded) — never any value.
	Secrets SetupSecrets `json:"secrets"`
	// AgeKey reports whether the at-rest secret store survives a restart.
	AgeKey SetupAgeKey `json:"age_key"`
	// HasRuns drives the wizard's "launch your first run" done state.
	HasRuns bool `json:"has_runs"`
	// Platform is the OS + WSL posture the environment-step copy keys off.
	Platform SetupPlatform `json:"platform"`
	// HostProxy is the host-side proxy detection (env/shell/git/tool-config/OS)
	// the Host Proxy Getting-Started step renders. Read-only detection; it
	// never configures anything (the upstream-proxy plumbing is separate).
	HostProxy setup.HostProxyDetection `json:"host_proxy"`
	// SCM is the presence-only git-credential posture (gh CLI login, helper,
	// plaintext stores) the ScmProviderStep's ladder recommendations key off.
	SCM setup.SCMPosture `json:"scm"`
	// Bedrock is the AWS Bedrock Anthropic-transport readiness the "Connect a
	// model" step renders alongside the API-key/subscription rows. Region/Model
	// are boot-time operator config (non-secret, safe to echo); the AWS
	// credentials themselves are never echoed — CredsPresent is a bool derived
	// from secret-name presence, same as every other secret in this contract.
	Bedrock SetupBedrock `json:"bedrock"`
	// Deployment reports whether wardynd itself sees a resident Claude login
	// (host mode) or is blind to it (compose/container).
	Deployment SetupDeployment `json:"deployment"`
	// Harness reports per-provider Wardyn-managed subscription credentials
	// captured via container login (setup-token), so the wizard can show a
	// "connected / expiring / reconnect" row that works in compose mode where
	// there is no resident host login. Empty when no managed credential exists.
	Harness []SetupHarness `json:"harness,omitempty"`
	// Integrations is the effective integration set (stored ∪ legacy-derived,
	// see effectiveIntegrations in integrations.go) with each row's live
	// capability matrix — the same shape GET /api/v1/integrations returns,
	// folded in here so the wizard needs one fewer round trip. ADDITIVE field;
	// omitted when empty.
	Integrations []SetupIntegration `json:"integrations,omitempty"`
	// Harnesses is the STATIC coding-agent harness catalog (harnessCatalog,
	// harness.go) — which tools Wardyn knows how to run and whether it can
	// wire each one a managed model credential or a container-login
	// subscription. Distinct from Harness above (a CAPTURED credential's live
	// readiness). ADDITIVE field; omitted when empty.
	Harnesses []SetupHarnessTool `json:"harnesses,omitempty"`
	// LLMReady is the server-computed "does SOME run/compose LLM access path
	// exist" verdict (HIGH-4 review fix) — the same winning-signal logic that
	// already decides llmProvenance's detail (resident CLI login, a real
	// composer backend, a secret-name heuristic, Bedrock, a managed harness
	// token) OR'd with an ai_provider Integration being configured. It exists
	// because a MEMBER'S redacted response (redactSetupStatusForMember) drops
	// the checks/providers/secret-name detail that would otherwise let the
	// console derive this itself — LLMReady is computed BEFORE redaction and
	// deliberately left untouched BY it, so the console's readiness chip / new-run
	// banner / demo gating keep working for a member without any of that detail
	// leaking. Kept in exact sync with ui/src/app/lib/types.ts's SetupStatus.
	LLMReady bool `json:"llm_ready"`
}

// SetupHarness is a Wardyn-managed subscription credential's readiness. Derived
// purely from the stored blob (presence + capture age) — PRESENCE only, honesty
// law: no green badge implies the token was live-verified. setup-token tokens
// live ~1yr with no machine-readable expiry, so Aging is a conservative
// age-based "reconnect soon" flag, never a hard expiry claim.
type SetupHarness struct {
	Provider    string `json:"provider"`              // "anthropic" | "aws"
	Captured    bool   `json:"captured"`              // a token blob is stored
	CapturedAt  string `json:"captured_at,omitempty"` // RFC3339, when pasted
	Aging       bool   `json:"aging,omitempty"`       // captured longer ago than harnessTokenAging
	SourceRunID string `json:"source_run_id,omitempty"`
	// ExpiresAt/Expired carry a REAL, machine-readable expiry and are populated
	// only for providers whose credential exposes one (AWS SSO does; an Anthropic
	// setup-token does not, which is the whole reason Aging exists as a
	// conservative age heuristic). Empty here means "this provider can't tell you"
	// — never "it doesn't expire".
	ExpiresAt string `json:"expires_at,omitempty"`
	Expired   bool   `json:"expired,omitempty"`
	// Renewable: the stored credential carries a refresh token, so it can be
	// renewed without a fresh interactive login (AWS `sso-session` profiles).
	// Legacy sso_start_url profiles have none and must be re-logged-in.
	Renewable bool `json:"renewable,omitempty"`
}

// harnessCredentialCheck is the readiness row for a Wardyn-managed subscription
// token — the compose-mode analogue of claudeSubscriptionStagingCheck. It fires
// only when a credential is captured (no capture => the llm_provider check
// already says "connect a model"). Pure: the blob read is done by the caller.
func harnessCredentialCheck(h SetupHarness) (SetupCheck, bool) {
	if !h.Captured {
		return SetupCheck{}, false
	}
	// AWS SSO carries a real expiry, so it gets a truthful row rather than the
	// age heuristic below (which exists only because setup-tokens expose none).
	if h.Provider == awsSSOProvider {
		switch {
		case h.Expired && h.Renewable:
			return SetupCheck{
				ID: "harness_credential_aws", Label: "AWS SSO session", Status: "warn",
				Detail: "Your captured AWS SSO session expired at " + h.ExpiresAt +
					". It carries a refresh token, so it can be renewed without logging in again.",
				Fix: "Re-run the containerized AWS SSO login on the provider step to refresh it.",
			}, true
		case h.Expired:
			return SetupCheck{
				ID: "harness_credential_aws", Label: "AWS SSO session", Status: "warn",
				Detail: "Your captured AWS SSO session expired at " + h.ExpiresAt +
					" and has no refresh token (legacy sso_start_url profile), so Bedrock runs using it will fail.",
				Fix: "Re-run the containerized AWS SSO login on the provider step.",
			}, true
		default:
			return SetupCheck{
				ID: "harness_credential_aws", Label: "AWS SSO session", Status: "ok",
				Detail: "A captured AWS SSO session is connected (expires " + h.ExpiresAt +
					"). Bedrock runs exchange it for short-lived role credentials — no host ~/.aws mount and no static keys.",
			}, true
		}
	}
	if h.Aging {
		return SetupCheck{
			ID: "harness_credential", Label: "Managed Claude subscription", Status: "warn",
			Detail: "Your Wardyn-managed Claude subscription token was captured a long time ago (setup-token lives ~1 year). " +
				"It may be close to expiring; a run will fail if Anthropic has revoked it.",
			Fix: "Reconnect via container login on the provider step (Connect via container login → `claude setup-token` → paste).",
		}, true
	}
	return SetupCheck{
		ID: "harness_credential", Label: "Managed Claude subscription", Status: "ok",
		Detail: "A Wardyn-managed Claude subscription token is connected and injected proxy-side into every run — the " +
			"sandbox holds only an inert sentinel. Works in compose mode with no host ~/.claude.",
	}, true
}

// SetupDeployment reports whether the wardynd process itself sees a resident
// Claude login — true in host mode (run-host.sh: wardynd runs as the operator,
// ~/.claude + the claude binary are on its own PATH/HOME), false in the compose
// path (distroless container blind to the host). HONEST framing like detectKVM:
// this is "does THIS process see a resident claude", not "is it literally
// run-host.sh" — a compose container with ~/.claude bind-mounted would also read
// host-like. The UI uses it to fork the getting-started guidance (laptop/local vs
// team/server) and to explain why the LLM-access check is or isn't green.
type SetupDeployment struct {
	HostLike bool `json:"host_like"`
}

// SetupBedrock (the Bedrock readiness snapshot) and its predicates live in
// runs_bedrock.go, next to the resolveBedrockAuth gate they must mirror.

// SetupCheck is one environment/readiness row. Status is ok|warn|fail|info;
// "info" is a permanent, non-fixable condition (e.g. no /dev/kvm on macOS) that
// must render as informational, not as a clearable warning. Platform lets the UI
// show environment-appropriate copy (linux|darwin|windows|wsl|any).
type SetupCheck struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Status   string `json:"status"`
	Platform string `json:"platform,omitempty"`
	Detail   string `json:"detail,omitempty"`
	Fix      string `json:"fix,omitempty"`
}

// SetupAuth is the active public-API auth mode: local (loopback bypass) | sso
// (OIDC) | token (admin bearer) | disabled (no auth configured, API closed).
type SetupAuth struct {
	Mode          string `json:"mode"`
	LocalLoopback bool   `json:"local_loopback"`
}

// SetupRunner echoes the runner name and the live confinement classes/substrates.
type SetupRunner struct {
	Driver                string            `json:"driver"`
	ConfinementClasses    []string          `json:"confinement_classes"`
	ConfinementSubstrates map[string]string `json:"confinement_substrates,omitempty"`
}

// SetupComposer is the composer enablement plus each configured backend's
// readiness (a BOOT snapshot, so it can surface disabled + needs-key states the
// live registry alone can't show).
type SetupComposer struct {
	Enabled  bool                       `json:"enabled"`
	Default  string                     `json:"default,omitempty"`
	Backends []ComposerBackendReadiness `json:"backends"`
}

// ComposerBackendReadiness is the boot-snapshot readiness of one configured
// composer backend. KeySecret is a secret NAME (never a value); KeyResolved is
// whether that secret (or the env fallback) was present at boot.
type ComposerBackendReadiness struct {
	Name        string `json:"name"`
	Provider    string `json:"provider"`
	Model       string `json:"model"`
	Wire        string `json:"wire"`
	Transport   string `json:"transport,omitempty"` // normalized (HTTP wires => "api"); cli tool / fake variant
	Auth        string `json:"auth,omitempty"`      // openai azure only: apikey|entra
	Enabled     bool   `json:"enabled"`
	NeedsKey    bool   `json:"needs_key"`
	KeySecret   string `json:"key_secret,omitempty"`
	KeyResolved bool   `json:"key_resolved"`
}

// SetupProvider is a resident coding-agent CLI (claude|codex) detected on PATH.
// LoggedIn is ADVISORY (a home-dir credential-file heuristic, not a live check).
type SetupProvider struct {
	Tool             string `json:"tool"`
	Installed        bool   `json:"installed"`
	LoggedIn         bool   `json:"logged_in"`
	LoginDetectedVia string `json:"login_detected_via,omitempty"`
	// AuthMode is how the CLI authenticates, when detectable: "subscription" (a
	// resident Claude OAuth token is present — fresh OR expired; freshness lives in
	// the llm_provider check Detail, not here) or "" (unknown; never guessed). The
	// "api_key" value is reserved in the contract but not inferred for a CLI (no
	// cheap honest signal); codex stays "" (no auth-file parse).
	AuthMode string `json:"auth_mode,omitempty"`
}

// SetupSecrets reports present secret NAMES (reserved names excluded) and a
// convenience bool for whether both GitHub App secrets are set.
type SetupSecrets struct {
	Present   []string `json:"present"`
	GitHubApp bool     `json:"github_app"`
}

// SetupAgeKey reports whether the secret store survives a restart (a stable
// WARDYN_AGE_KEY was supplied vs an ephemeral generated one).
type SetupAgeKey struct {
	Durable bool `json:"durable"`
}

// SetupPlatform is the wardynd host's OS + WSL posture.
type SetupPlatform struct {
	OS  string `json:"os"`
	WSL bool   `json:"wsl"`
	// KVM: the host exposes /dev/kvm — lets the UI split Vault's "incompatible
	// with this hardware" from a fixable "needs setup" (additive; old UIs ignore).
	KVM bool `json:"kvm"`
}

// deploymentHostLike reports whether the claude provider in providers is both
// installed and logged in — the same signal llmProvenance treats as a resident
// CLI login, reused here (no new host I/O) to answer "does wardynd itself see a
// resident Claude login" for SetupDeployment.HostLike. Extracted pure so it is
// unit-testable without host CLI detection.
func deploymentHostLike(providers []SetupProvider) bool {
	for _, p := range providers {
		if p.Tool == "claude" {
			return p.Installed && p.LoggedIn
		}
	}
	return false
}

// llmProvenance is the single LLM-access predicate: it returns the human detail
// for the WINNING signal (resident CLI login > enabled real composer backend >
// api-key-ish secret) and "" when none is present — readiness is simply
// "llmProvenance != \"\"", so the boolean and the rendered detail can never drift.
//
// The `fake` exclusion is the honesty guard: a `wire:"fake"` backend resolves
// trivially (it needs no key) but calls no model, so counting it would render an
// "LLM access ✓" for a user whose only backend is the demo stub (exactly the
// default `make setup` config) — a lie.
//
// claudeDetail is the precomputed subscription-aware sentence for a resident
// Claude CLI login (see subscriptionLLMDetail); it is used only when a logged-in
// claude CLI is the winner, and falls back to a generic sentence when empty (the
// subscription provider was unwired, so no peek was possible).
func llmProvenance(providers []SetupProvider, backends []ComposerBackendReadiness, secretNames []string, claudeDetail string) string {
	for _, p := range providers {
		// A logged-in CLI is real access; merely installed-but-not-logged-in is not.
		if !p.LoggedIn {
			continue
		}
		if p.Tool == "claude" && claudeDetail != "" {
			return claudeDetail
		}
		return fmt.Sprintf("Resident %s CLI is logged in (advisory: a credential file is present).", p.Tool)
	}
	for _, b := range backends {
		if b.Enabled && b.KeyResolved && b.Wire != "fake" {
			return fmt.Sprintf("AI Run Composer backend %q (%s) has a resolved API key.", b.Name, b.Provider)
		}
	}
	for _, n := range secretNames {
		l := strings.ToLower(n)
		if strings.Contains(l, "api") || strings.Contains(l, "anthropic") || strings.Contains(l, "openai") {
			return fmt.Sprintf("An LLM API key secret (%q) is present.", n)
		}
	}
	return ""
}

// computeLLMReady is SetupStatus.LLMReady's verdict (HIGH-4 review fix),
// pulled out of handleSetupStatus as its own pure function purely to keep
// that handler's branching under the gocyclo gate — llmDetail already IS
// llmProvenance's own winning signal (plus Bedrock/managed-harness, folded in
// by the caller before this runs), so the only new branching here is the
// ai_provider Integration fallback for when llmDetail came up empty.
func computeLLMReady(llmDetail string, integrations []SetupIntegration) bool {
	if llmDetail != "" {
		return true
	}
	for _, in := range integrations {
		if in.Category == types.IntegrationAIProvider {
			return true
		}
	}
	return false
}

// subscriptionLLMDetail composes the LLM-access detail for a resident Claude Code
// CLI login, distinguishing a live Claude SUBSCRIPTION (a peeked OAuth token) from
// a fresh/expired token and folding in whether subscription runs inject the live
// host token or fall back to the mounted copy. Pure: all host I/O (the read-only
// Peek, the PATH lookup) is done by the caller and passed in. loginVia is the
// credential path the login heuristic matched; binPath is the resolved `claude`
// binary ("" when logged in but off PATH). It intentionally never embeds a
// credentials-file path in copy (the honesty note's "derive from CredPath, never
// hardcode" is met by not naming the file at all).
func subscriptionLLMDetail(tok subscription.Token, peekErr error, injectEnabled bool, loginVia, binPath string, now time.Time) string {
	// No readable subscription OAuth token: the login heuristic fired on some other
	// credential (an API-key session, or a stale/absent creds file), not a
	// subscription. The CLI login still counts as access — we just can't confirm a
	// subscription token.
	if peekErr != nil || tok.Value == "" {
		via := ""
		if loginVia != "" {
			via = " (via " + loginVia + ")"
		}
		return "Claude Code CLI login detected" + via + "; no readable Claude subscription token."
	}
	var b strings.Builder
	b.WriteString("Claude Code CLI signed in with a Claude subscription")
	if tok.ExpiresAt.After(now) {
		b.WriteString(" (subscription token valid)")
	} else {
		b.WriteString(" (subscription token EXPIRED — run `claude` on the host to refresh)")
	}
	if binPath == "" {
		// Logged in, but the resident CLI is off PATH, so the provider cannot
		// delegate a refresh here (Current would fail closed near expiry).
		b.WriteString("; the `claude` CLI is not on PATH, so its token cannot be refreshed here")
	}
	if injectEnabled {
		b.WriteString("; subscription runs inject a fresh host token proxy-side")
	} else {
		b.WriteString("; subscription injection is off — runs use the mounted credential copy")
	}
	b.WriteString(".")
	return b.String()
}

// llmCeilingAdmits reports whether the DefaultPolicy ceiling would let a COMPOSED
// run actually reach provider p's model, given which agent credentials are present.
// It mirrors the EXACT predicates compose applies (ensureLLMGrant adds the grant +
// exact-host egress; clampGrants drops any grant KIND absent from the ceiling and
// force-tightens approval; reconcileLLMAccess's positive note) so this setup check
// can never disagree with what the clamp actually does at compose time.
//   - api-key path: the ceiling egress-allows p.host AND carries an auto-mint
//     (non-approval) api_key grant KIND (clampGrants matches by kind, any host).
//   - subscription path (Claude only): the ceiling blesses the /home/agent/.claude
//     mount AND allows api.anthropic.com egress (applyLLMCredMount's gates).
func llmCeilingAdmits(ceiling types.RunPolicySpec, p llmProvider, hasKey, hasSub bool) bool {
	if hasKey {
		hostAllowed := ceiling.AllowAllEgress || domainAllowedExact(ceiling.AllowedDomains, p.host)
		if hostAllowed && slices.ContainsFunc(ceiling.EligibleGrants, func(g types.GrantSpec) bool {
			return g.Kind == types.GrantAPIKey && !g.RequiresApproval
		}) {
			return true
		}
	}
	if hasSub && p.host == "api.anthropic.com" && ceilingBlessesClaudeCreds(ceiling) && anthropicReachable(&ceiling) {
		return true
	}
	return false
}

// composerCeilingCheck is the "will a composed run actually reach the model"
// readiness row. It fires ONLY when the operator already has an agent credential (a
// stored anthropic/openai key, or a resident Claude subscription login) — when none
// is present the llm_provider check already says "add one", so this would be noise.
// The gap it catches: a credential is stored and every other check reads green, yet
// the DefaultPolicy ceiling won't broker it, so a first composed run boots and 404s
// on its first model call. Pure (host I/O done by the caller) so it is unit-testable.
func composerCeilingCheck(ceiling types.RunPolicySpec, hasAnthropicKey, hasOpenAIKey, hasClaudeSub bool) (SetupCheck, bool) {
	type cred struct {
		agent  string
		label  string
		hasKey bool
		hasSub bool
	}
	var creds []cred
	if hasAnthropicKey || hasClaudeSub {
		creds = append(creds, cred{agent: "claude-code", label: "Anthropic (Claude)", hasKey: hasAnthropicKey, hasSub: hasClaudeSub})
	}
	if hasOpenAIKey {
		creds = append(creds, cred{agent: "codex-cli", label: "OpenAI (Codex)", hasKey: hasOpenAIKey})
	}
	if len(creds) == 0 {
		return SetupCheck{}, false
	}
	var blocked, admitted []string
	for _, c := range creds {
		p, ok := agentLLMProvider(c.agent)
		if !ok {
			continue
		}
		if llmCeilingAdmits(ceiling, p, c.hasKey, c.hasSub) {
			admitted = append(admitted, c.label)
		} else {
			blocked = append(blocked, c.label)
		}
	}
	if len(blocked) == 0 {
		return SetupCheck{
			ID: "composer_llm_ceiling", Label: "Model access for composed runs", Status: "ok",
			Detail: "The default policy brokers model access for a composed run (" + strings.Join(admitted, ", ") + ").",
		}, true
	}
	return SetupCheck{
		ID: "composer_llm_ceiling", Label: "Model access for composed runs", Status: "warn",
		Detail: "A credential for " + strings.Join(blocked, ", ") + " is stored, but WARDYN_DEFAULT_POLICY does not broker an " +
			"auto-mint api_key grant with matching egress (or bless a Claude credential mount) — so a composed run's first " +
			"model call will 404 even though every credential check reads green.",
		Fix: "Point WARDYN_DEFAULT_POLICY (helm: env.WARDYN_DEFAULT_POLICY) at a composer-capable ceiling (e.g. " +
			"examples/policies/composer-dev.json) and restart wardynd. `make setup` now auto-picks it when a real model path is configured.",
	}, true
}

// claudeSubscriptionStagingCheck is the "will a resident-host Claude
// subscription integration actually work" readiness row. It fires ONLY when a
// resident Claude login is detected (no login => the llm_provider check
// already says "add one"). The gap it catches: the model-access badge reads
// green from the HOST login, but a run only reaches it after staging generates
// the subscription ceiling (~/.wardyn/composer-dev-subscription.json) and
// wardynd restarts onto it — a headless `make setup` (no TTY, no
// WARDYN_STAGE_CLAUDE=1) skips staging silently. blessed mirrors run-host.sh's
// policy pick: WARDYN_DEFAULT_POLICY blesses the /home/agent/.claude mount only
// when staging produced the ceiling, so logged-in && !blessed == "not staged".
// Pure (host I/O done by the caller) so it is unit-testable.
func claudeSubscriptionStagingCheck(hasClaudeSub, blessed bool, loginVia string) (SetupCheck, bool) {
	if !hasClaudeSub {
		return SetupCheck{}, false
	}
	if blessed {
		return SetupCheck{
			ID: "claude_subscription_staging", Label: "Claude subscription staging", Status: "ok",
			Detail: "Your Claude login is staged for sandbox use — a run picks it up once its resolved integration is " +
				"a resident-host Claude subscription. Adopt the derived \"Claude subscription (resident host)\" " +
				"integration under Integrations and mark it the agent-runs default, or pin it on a workspace's Model access.",
		}, true
	}
	fix := "Run `make stage-claude` on the host — it stages the login and restarts wardynd onto the subscription ceiling."
	if strings.Contains(loginVia, "Keychain") {
		fix = "Your Claude login lives in the macOS Keychain, which staging cannot read. Run `claude login` once over SSH " +
			"(it writes ~/.claude/.credentials.json), then `make stage-claude`."
	}
	return SetupCheck{
		ID: "claude_subscription_staging", Label: "Claude subscription staging", Status: "warn",
		Detail: "A resident Claude login was detected — the model-access badge is green — but it is NOT staged for " +
			"sandbox use, so a run whose resolved integration is a resident-host Claude subscription (the agent-runs " +
			"default, or a workspace's Model access pin) can't reach it.",
		Fix: fix,
	}, true
}

// agentImageCheck reports the resolved claude-code agent image so an operator
// sees, before a run ever fails, whether it is the Node-only convention image
// or a provisioned override — the readiness surface for the multi-toolchain image's
// BLOCKER-1 (a non-JS workspace exit-127s on the shipped default, silently).
// wardynd has no docker CLI (the compose build is distroless static) and no
// wired image-inspect capability on the Runner interface, so this is a NAME
// heuristic against the two known-Node-only convention refs, not a real
// `docker inspect` — labeled honestly as such rather than guessing further.
// Always info/warn, never fail: an operator-chosen image is assumed
// provisioned on purpose.
func agentImageCheck(images map[string]string) SetupCheck {
	ref := agentImage("claude-code", images)
	if isConventionNodeOnlyImage(ref) {
		return SetupCheck{
			ID: "agent_image", Label: "Agent image toolchains", Status: "warn",
			Detail: "The configured claude-code agent image (" + ref + ") is the Node-only convention image — " +
				"a non-JS workspace (Go/Rust/Java/Python) will fail verify/record with exit 127 (toolchain not found).",
			Fix: "Wire a multi-toolchain image via WARDYN_AGENT_IMAGES (helm: env.WARDYN_AGENT_IMAGES) (e.g. build deploy/images/full " +
				"(the fat toolchain image), or your own image satisfying the IMAGE CONTRACT in deploy/images/README.md), or pass a " +
				"per-run base image in the New Run wizard's \"Sandbox image\" field — Wardyn wraps it with the runner tools.",
		}
	}
	return SetupCheck{
		ID: "agent_image", Label: "Agent image toolchains", Status: "info",
		Detail: "Configured claude-code agent image: " + ref + ". Wardyn cannot inspect image contents from the " +
			"control plane (no docker CLI in the distroless build) — verify a workspace to confirm its toolchains.",
	}
}

// isConventionNodeOnlyImage reports whether ref is a shipped claude-code
// convention image (the ghcr fallback or a locally-built tag) — the images
// known, by construction, to carry Node only (deploy/images/claude-code/Dockerfile).
// The pre-rename :demo tag stays matched so holdout boxes keep the accurate warn.
func isConventionNodeOnlyImage(ref string) bool {
	return ref == "ghcr.io/cjohnstoniv/agent-claude-code:latest" ||
		ref == "wardyn/agent-claude-code:local" ||
		ref == "wardyn/agent-claude-code:demo"
}

// handleSetupStatus assembles the first-run readiness snapshot. It sits behind
// humanOrAdminAuth (reaching it already proves auth: local-mode bypass, an OIDC
// session, or the admin bearer), so it may enumerate resident CLIs, present
// secret names, and per-backend composer readiness — capability disclosure that
// must never appear on the public /healthz.
//
// The handler gathers state; every checklist row is a small pure function below
// (one per item, in the order the wizard renders them).
func (s *Server) handleSetupStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// auth: same derivation handleMe uses, plus the "disabled" edge (no auth
	// configured at all — practically unreachable here since adminAuth would have
	// 401'd, but kept honest against the frozen contract).
	authMode := "token"
	switch {
	case s.cfg.LocalMode:
		authMode = "local"
	case oidc.PrincipalFromContext(ctx) != "":
		authMode = "sso"
	case s.cfg.AdminToken == "":
		authMode = "disabled"
	}

	rnr, k8sNetpolProven := setupRunnerInfo(ctx, s.cfg.Runner)

	// composer: enablement + boot-snapshot backends.
	comp := SetupComposer{Backends: s.cfg.ComposerBackends}
	if comp.Backends == nil {
		comp.Backends = []ComposerBackendReadiness{}
	}
	if s.cfg.Composer != nil && s.cfg.Composer.Enabled() {
		comp.Enabled = true
		comp.Default = s.cfg.Composer.Default()
	}

	providers, claudeDetail := s.setupProviders()

	// secrets: names only (reserved excluded); github_app iff both App secrets present.
	secretNames := []string{}
	if s.cfg.Secrets != nil {
		names, err := s.listUserSecretNames(ctx)
		if err != nil {
			writeError(w, http.StatusInternalServerError, "list secrets: "+err.Error())
			return
		}
		secretNames = names
	}
	present := make(map[string]bool, len(secretNames))
	for _, n := range secretNames {
		present[n] = true
	}
	sec := SetupSecrets{
		Present:   secretNames,
		GitHubApp: present[secretGitHubAppID] && present[secretGitHubAppKey],
	}

	plat := setup.DetectPlatform()
	hostProxy := setup.DetectHostProxy()
	scmPosture := setup.DetectSCMPosture()

	// LLM access provenance: the detail of the WINNING signal (resident CLI login,
	// a REAL non-fake composer backend, or an api-key-ish secret), "" when none.
	// the secret-name scan is a loose substring signal; the exact truth
	// (a working model call) is only known at run time — this just decides whether
	// to warn the operator up front. See llmProvenance for the honesty guard.
	llmDetail := llmProvenance(providers, comp.Backends, secretNames, claudeDetail)

	// Bedrock readiness: region/model are boot-time config (non-secret, safe to
	// echo to the UI); CredsPresent mirrors resolveBedrockAuth's secret-name
	// check (presence, not the value). Folded into llmDetail as an ADDITIONAL
	// winning signal (not a change to llmProvenance's own priority order) so a
	// Bedrock-only operator still sees "LLM access: ok" without touching the
	// existing CLI/composer/secret-name signals or their tests.
	bedrock := s.setupBedrock(ctx, present)
	if llmDetail == "" && bedrock.Ready {
		llmDetail = fmt.Sprintf(
			"AWS Bedrock is configured (region %s, model %s); Claude runs authenticate via %s.",
			bedrock.Region, bedrock.Model, bedrock.credSourceDesc())
	}

	harnessCreds, managedDetail := s.setupHarnessCreds(ctx)
	if llmDetail == "" {
		llmDetail = managedDetail
	}

	// llm_ready (HIGH-4 review fix): llmDetail's own winning signal (resident
	// CLI login, a real composer backend, a secret-name heuristic, Bedrock, or
	// a managed harness token — everything folded in above) OR'd with an
	// ai_provider Integration being configured, computed ONCE here and reused
	// below for resp.Integrations so effectiveIntegrations() is not walked
	// twice. Computed BEFORE redaction and left untouched by it (see
	// redactSetupStatusForMember) — a member's console needs the ANSWER even
	// though it can no longer see the detail that produced it.
	//
	// PLATFORM-API-7: use the *Using form, reusing the present/providers/bedrock
	// already computed above, so this single call does NOT redo a full secret
	// listing, a CLI sweep + subscription peek, and an AWS-SSO-blob age decrypt.
	integrations := s.integrationsWithCapabilitiesUsing(ctx, present, providers, bedrock)
	llmReady := computeLLMReady(llmDetail, integrations)

	// checks: the rows the wizard renders. "info" is used for permanent /
	// non-fixable or purely-optional conditions so the user is never shown a red
	// they cannot clear.
	checks := []SetupCheck{
		runnerCheck(rnr),
		agentImageCheck(s.cfg.AgentImages),
		envBuilderCheck(s.cfg.ImageBuilder != nil),
		llmProviderCheck(llmDetail),
	}
	// k8s_egress_containment: the boot-time NetworkPolicy canary verdict —
	// absent (no row) on a non-k8s driver; see k8sEgressContainmentCheck.
	if chk, ok := k8sEgressContainmentCheck(rnr.Driver, k8sNetpolProven); ok {
		checks = append(checks, chk)
	}
	if chk, ok := bedrockProviderCheck(bedrock); ok {
		checks = append(checks, chk)
	}

	// composer_llm_ceiling: a credential is present but does the DEFAULT POLICY
	// ceiling actually let a COMPOSED run use it? Catches the "everything green, first
	// run 404s" trap where WARDYN_DEFAULT_POLICY (e.g. demo.json/default.json) carries
	// no api_key grant. A resident Claude CLI login signals the subscription path.
	hasClaudeSub, claudeLoginVia := claudeLoginSignal(providers)
	if chk, ok := composerCeilingCheck(s.cfg.DefaultPolicy, present["anthropic-api-key"], present["openai-api-key"], hasClaudeSub); ok {
		checks = append(checks, chk)
	}

	// claude_subscription_staging: the login is detected, but is it STAGED so a
	// resident-host subscription integration can actually reach it? Catches the
	// headless-`make setup` skip where the badge is green yet a run resolved to
	// that integration gets nothing.
	if chk, ok := claudeSubscriptionStagingCheck(hasClaudeSub, ceilingBlessesClaudeCreds(s.cfg.DefaultPolicy), claudeLoginVia); ok {
		checks = append(checks, chk)
	}

	// harness_credential: the compose-mode analogue — a Wardyn-managed subscription
	// token captured via container login, with an age-based "reconnect" warning.
	for _, h := range harnessCreds {
		if chk, ok := harnessCredentialCheck(h); ok {
			checks = append(checks, chk)
		}
	}

	checks = append(checks, composerCheck(comp), ageKeyCheck(s.cfg.AgeKeyDurable),
		hostProxyCheck(hostProxy, plat.Containerized && !setup.HostProxySeeded()))

	// sso_rbac / tls_cookie_posture: both OIDC-gated (mirror how every other
	// conditional check gates on its own applicability).
	oidcConfigured := s.cfg.OIDC != nil
	if chk, ok := ssoRBACCheck(oidcConfigured, s.cfg.OIDCRoleMapConfigured); ok {
		checks = append(checks, chk)
	}
	if chk, ok := tlsCookiePostureCheck(oidcConfigured, s.cfg.OIDCRedirectURL, s.cfg.OIDCSecureCookies); ok {
		checks = append(checks, chk)
	}

	if s.cfg.Store != nil {
		if sc, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
			checks = append(checks, siteConfigCheck(sc), artifactRepoCheck(sc))
		}
	}

	checks = append(checks, scmProviderCheck(sec.GitHubApp, secretNames, scmPosture))

	// github_ref_ruleset: the only row that leaves the machine. Gated on the App
	// being configured, cached, short-timeout, and never worse than "warn" — see
	// githubRefRulesetCheck.
	if chk, ok := s.githubRefRulesetCheck(ctx, sec.GitHubApp); ok {
		checks = append(checks, chk)
	}
	checks = append(checks, platformChecks(plat)...)

	// has_runs: cheap existence check via the store. reuses ListRuns (fine for a
	// first-run wizard); a dedicated COUNT(*)/EXISTS is the upgrade if run volume
	// ever makes this scan matter.
	hasRuns := false
	if s.cfg.Store != nil {
		runs, err := s.cfg.Store.ListRuns(ctx)
		hasRuns = err == nil && len(runs) > 0
	}

	// ready: CONSERVATIVE — false when the runner is nil / has no live class, so
	// the wizard opens rather than hiding a half-configured bootstrap. Composer /
	// credentials are warnings, not readiness gates.
	ready := s.cfg.Runner != nil && len(rnr.ConfinementClasses) > 0

	resp := SetupStatus{
		Ready:      ready,
		Checks:     checks,
		Auth:       SetupAuth{Mode: authMode, LocalLoopback: s.cfg.LocalLoopback},
		Runner:     rnr,
		Composer:   comp,
		Providers:  providers,
		Secrets:    sec,
		AgeKey:     SetupAgeKey{Durable: s.cfg.AgeKeyDurable},
		HasRuns:    hasRuns,
		Platform:   SetupPlatform{OS: plat.OS, WSL: plat.WSL, KVM: plat.KVM},
		HostProxy:  hostProxy,
		SCM:        scmPosture,
		Bedrock:    bedrock,
		Deployment: SetupDeployment{HostLike: deploymentHostLike(providers)},
		Harness:    harnessCreds,
		// Integrations reuses the single integrationsWithCapabilitiesUsing call
		// hoisted above (PLATFORM-API-7 optimization + HIGH-4 llm_ready reuse).
		Integrations: integrations,
		Harnesses:    setupHarnessTools(),
		LLMReady:     llmReady,
	}
	if !s.isOperator(ctx) {
		resp = redactSetupStatusForMember(resp)
	}
	writeJSON(w, http.StatusOK, resp)
}

// redactSetupStatusForMember drops the operator/admin-facing DIAGNOSTIC detail
// a member has no route to act on — the environment/credential checklist rows,
// resident-CLI login detection, and secret NAMES (item 2's explicit drop list:
// checks/providers/secret names/runner detail) — while keeping everything a
// member's own console needs: Ready/Auth (App.tsx's reachability gate) and
// HasRuns, plus every field the run-launch/compose UI reads (Composer,
// Bedrock, Deployment, Harness*, Integrations, Platform, HostProxy, SCM,
// AgeKey) so a member can still launch and compose runs normally. This only
// ZEROES fields on an already-computed, already-200 response — it can never
// itself produce an error state (no non-401 error is possible for a member
// here, by construction).
func redactSetupStatusForMember(st SetupStatus) SetupStatus {
	st.Checks = []SetupCheck{}
	st.Providers = []SetupProvider{}
	st.Secrets = SetupSecrets{Present: []string{}}
	st.Runner = SetupRunner{ConfinementClasses: []string{}}
	return st
}

// setupProviders detects the resident coding-agent CLIs and returns them plus
// the subscription-aware detail for a logged-in claude CLI ("" when there is
// none). It peeks the resident Claude subscription OAuth token (read-only; never
// refreshes) so the claude row can carry auth_mode "subscription" — set whenever
// a token is present, fresh OR expired, because freshness belongs in the detail,
// not the mode. Skipped when no subscription provider is wired (tests /
// unconfigured host).
func (s *Server) setupProviders() ([]SetupProvider, string) {
	var subTok subscription.Token
	var subPeekErr error
	subWired := s.cfg.SubscriptionToken != nil
	if subWired {
		subTok, subPeekErr = s.cfg.SubscriptionToken.Peek()
	}
	subOK := subWired && subPeekErr == nil && subTok.Value != ""

	provs := setup.DetectCLIProviders()
	providers := make([]SetupProvider, 0, len(provs))
	claudeDetail := ""
	for _, p := range provs {
		sp := SetupProvider{
			Tool: p.Tool, Installed: p.Installed, LoggedIn: p.LoggedIn, LoginDetectedVia: p.LoginVia,
		}
		if p.Tool == "claude" && subWired {
			if subOK {
				sp.AuthMode = "subscription"
			}
			if p.LoggedIn {
				claudeDetail = subscriptionLLMDetail(subTok, subPeekErr, s.subscriptionInjectEnabled(), p.LoginVia, p.BinPath, s.cfg.Now())
			}
		}
		providers = append(providers, sp)
	}
	return providers, claudeDetail
}

// claudeLoginSignal reports whether a resident claude CLI is logged in (the
// subscription-path signal) and how that login was detected.
func claudeLoginSignal(providers []SetupProvider) (bool, string) {
	for _, p := range providers {
		if p.Tool == "claude" && p.LoggedIn {
			return true, p.LoginDetectedVia
		}
	}
	return false, ""
}

// setupHarnessCreds reports the credentials captured by a containerized login,
// plus the llm-access detail the MANAGED subscription contributes ("" when there
// is none) — a compose-mode LLM source with no resident host login, folded in by
// the caller as an ADDITIONAL winning signal so a managed-only operator reads
// "LLM access: ok" without changing llmProvenance's own priority order. The AWS
// SSO entry is deliberately NOT folded in: it credentials Bedrock specifically,
// and the bedrock_provider check already owns that story. It reports TRUE expiry
// rather than the managed token's age heuristic.
func (s *Server) setupHarnessCreds(ctx context.Context) ([]SetupHarness, string) {
	var out []SetupHarness
	managedDetail := ""
	if blob, ok, err := s.readManagedBlob(ctx, "anthropic"); err == nil && ok {
		out = append(out, SetupHarness{
			Provider: "anthropic", Captured: true,
			CapturedAt:  blob.CapturedAt.Format(time.RFC3339),
			Aging:       s.cfg.Now().UTC().Sub(blob.CapturedAt) > harnessTokenAging,
			SourceRunID: blob.SourceRunID,
		})
		managedDetail = "A Wardyn-managed Claude subscription token (captured via container login) is injected proxy-side into every run."
	}
	if blob, ok, err := s.readAWSSSOBlob(ctx); err == nil && ok {
		out = append(out, SetupHarness{
			Provider: awsSSOProvider, Captured: true,
			CapturedAt:  blob.CapturedAt.Format(time.RFC3339),
			ExpiresAt:   blob.ExpiresAt.Format(time.RFC3339),
			Expired:     blob.expired(s.cfg.Now().UTC()),
			Renewable:   blob.RefreshToken != "",
			SourceRunID: blob.SourceRunID,
		})
	}
	return out, managedDetail
}

// setupRunnerInfo reports the live runner selection for /setup/status, copying
// handleHealthz's Capabilities pattern: a nil runner (or one whose Capabilities
// call fails) reads honestly as "none" with no classes rather than claiming
// isolation this host cannot deliver.
//
// The second return value is the k8s substrate's boot-time egress-canary
// verdict ("enforced"/"unenforced"/"") — computed here (it needs the SAME
// Capabilities() call this function already makes) but deliberately NOT part
// of the SetupRunner struct: nothing on the wire reads it, only the caller's
// k8sEgressContainmentCheck (setup_checks.go), fed directly, a local value is
// enough. "" covers three real cases, not two: a non-k8s driver; a k8s daemon
// build that predates this computation; AND a genuine k8s driver whose
// Capabilities() call itself just errored (the `err != nil` return below) —
// that third case is not hypothetical, it is this very function's own
// early-return path, which already leaves Driver "k8s" with nothing else
// filled in. k8sEgressContainmentCheck grades all three the same honest way
// (Indeterminate/FAIL, never a silent Enforcing).
func setupRunnerInfo(ctx context.Context, rn runner.Runner) (SetupRunner, string) {
	out := SetupRunner{Driver: "none", ConfinementClasses: []string{}}
	if rn == nil {
		return out, ""
	}
	out.Driver = rn.Name()
	c, err := rn.Capabilities(ctx)
	if err != nil {
		return out, ""
	}
	for _, cc := range c.ConfinementClasses {
		out.ConfinementClasses = append(out.ConfinementClasses, string(cc))
	}
	if len(c.Resolved) > 0 {
		out.ConfinementSubstrates = make(map[string]string, len(c.Resolved))
		for k, v := range c.Resolved {
			out.ConfinementSubstrates[string(k)] = v
		}
	}
	netpolProven := ""
	if out.Driver == "k8s" {
		// c.NetworkPolicy is the orchestrator-aggregated ClassSupport signal;
		// "unenforced" is the only non-enforced verdict a LIVE daemon can ever
		// report here — an unenforced-without-override or a genuinely
		// indeterminate canary both refuse to boot entirely
		// (internal/runner/k8s's newWithClient).
		if c.NetworkPolicy {
			netpolProven = "enforced"
		} else {
			netpolProven = "unenforced"
		}
	}
	return out, netpolProven
}

// refRulesetTTL is how long one github_ref_ruleset answer is reused. The wizard
// polls /setup/status; without this every poll would be an api.github.com round
// trip and, on a busy installation, a rate-limit.
const refRulesetTTL = 5 * time.Minute

// refRulesetTimeout bounds the whole outbound probe (a token mint, up to two
// rule reads, one ruleset read per ruleset found, and a token revoke). Short on
// purpose: this row is advisory, and handleSetupStatus is a page load.
const refRulesetTimeout = 5 * time.Second

// githubRefRulesetCheck asks GitHub whether the App is actually ref-confined on
// a granted repo, and caches the answer for refRulesetTTL.
//
// This is the ONLY setup check that leaves the machine — every other row is
// local inspection (secret NAMES, env/file detection, the control plane's own
// Postgres, a runner capability probe). Three things keep that from being a
// regression:
//
//   - It is skipped entirely unless a GitHub App is configured AND a verifier is
//     wired AND a concrete repo is known — see firstBrokeredRepo for the two
//     places that can come from. A deployment with neither signal never makes
//     the call and never sees the row.
//   - Every failure — timeout, rate limit, 403 on the permission, a repo the
//     installation cannot see — grades "info"/unknown. A network blip must not
//     read as a security regression.
//   - The result is cached, so the wizard's polling cannot amplify it.
//
// It never grades "fail": like scmProviderCheck, it is not a gate. The gate is
// the opt-in broker.envRequireRefRuleset, and it lives in the mint path.
func (s *Server) githubRefRulesetCheck(ctx context.Context, githubApp bool) (SetupCheck, bool) {
	if !githubApp || s.cfg.GitHubRulesets == nil {
		return SetupCheck{}, false
	}
	s.refRulesetMu.Lock()
	defer s.refRulesetMu.Unlock()
	if !s.refRulesetAt.IsZero() && s.cfg.Now().Sub(s.refRulesetAt) < refRulesetTTL {
		return s.refRulesetRow, s.refRulesetShow
	}

	repo := s.firstBrokeredRepo(ctx)
	if repo == "" {
		s.refRulesetAt, s.refRulesetRow, s.refRulesetShow = s.cfg.Now(), SetupCheck{}, false
		return SetupCheck{}, false
	}
	probeCtx, cancel := context.WithTimeout(ctx, refRulesetTimeout)
	defer cancel()
	confined, detail, err := s.cfg.GitHubRulesets.VerifyRefRuleset(probeCtx, repo)

	s.refRulesetAt = s.cfg.Now()
	s.refRulesetRow = refRulesetCheck(repo, confined, detail, err)
	s.refRulesetShow = true
	return s.refRulesetRow, true
}

// firstBrokeredRepo returns the first "owner/name" Wardyn can name a concrete
// repo for. It tries the default policy, then any stored policy, for a
// github_token grant whose scope.repos is non-empty — a deliberate,
// admin-authored signal, but rare in practice: shipped example policies
// (examples/policies/*.json) all carry "repos": [] because eligible_grants
// are TEMPLATES the run fills in. Falls back to firstBrokeredRepoFromRuns
// (setup_checks.go) for the common case that leaves: a repo declared on an
// actual run, not a static policy field. "" when neither source has
// anything: a fresh install has nothing to probe, and the row is omitted
// rather than guessed at.
func (s *Server) firstBrokeredRepo(ctx context.Context) string {
	specs := []types.RunPolicySpec{s.cfg.DefaultPolicy}
	if s.cfg.Store != nil {
		if pols, err := s.cfg.Store.ListPolicies(ctx); err == nil {
			for _, p := range pols {
				specs = append(specs, p.Spec)
			}
		}
	}
	for _, spec := range specs {
		for _, g := range spec.EligibleGrants {
			if g.Kind != types.GrantGitHubToken {
				continue
			}
			if repos := githubScopeRepos(g.Scope); len(repos) > 0 {
				return repos[0]
			}
		}
	}
	return s.firstBrokeredRepoFromRuns(ctx)
}
