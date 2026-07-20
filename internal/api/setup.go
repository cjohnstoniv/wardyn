// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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
//
// The handler (handleSetupStatus), its route registration inside the
// humanOrAdminAuth group, provider/platform detection (internal/setup), and the
// ready computation are implemented in Workstream B. This file (Task 0) only
// freezes the wire types so B and the UI build against one shape.

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

// SetupBedrock is the Amazon Bedrock Anthropic-transport readiness snapshot.
type SetupBedrock struct {
	Region string `json:"region,omitempty"`
	Model  string `json:"model,omitempty"`
	// The three credential SOURCES resolveBedrockAuth accepts, in its precedence
	// order (bearer > ~/.aws mount > resident SigV4). ANY one is sufficient — a
	// mount- or bearer-configured host has NO aws-access-key-id/-secret secrets
	// yet is fully ready, so gating readiness on CredsPresent alone wrongly reads
	// "needs setup".
	CredsPresent  bool `json:"creds_present"`  // resident aws-access-key-id + aws-secret-access-key secrets
	AWSMount      bool `json:"aws_mount"`      // host-mode read-only ~/.aws bind-mount (SSO auto-refreshes)
	BearerPresent bool `json:"bearer_present"` // bedrock-api-key bearer token secret (never resident)
	// Ready is the server-computed readiness (region+model+any credential source),
	// echoed so the UI doesn't re-derive — and drift from — this gate.
	Ready bool `json:"ready"`
}

// bedrockReady reports whether a claude-code run would actually get the Bedrock
// transport right now — mirrors resolveBedrockAuth's gate: region + model AND at
// least one credential source (resident keys, a ~/.aws mount, or a bearer token).
// Presence, not value, is enough here (no live secret-store read).
func (b SetupBedrock) ready() bool {
	return b.Region != "" && b.Model != "" && (b.CredsPresent || b.AWSMount || b.BearerPresent)
}

// bedrockConfigured reports whether the operator has touched ANY Bedrock knob
// (region, model, or any credential source) — used to decide whether the
// bedrock_provider check is worth showing at all vs. staying silent for the
// overwhelming majority of operators who never use Bedrock.
func (b SetupBedrock) configured() bool {
	return b.Region != "" || b.Model != "" || b.CredsPresent || b.AWSMount || b.BearerPresent
}

// credSourceDesc names the winning credential source (resolveBedrockAuth's
// precedence) for honest UI copy — "resident keys" is wrong for a mount/bearer host.
func (b SetupBedrock) credSourceDesc() string {
	switch {
	case b.BearerPresent:
		return "a proxy-injected Bedrock API key (never resident in the sandbox)"
	case b.AWSMount:
		return "your host AWS credentials via a read-only ~/.aws mount (SSO auto-refreshes)"
	default:
		return "resident AWS SigV4 credentials"
	}
}

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
		if hostAllowed {
			for _, g := range ceiling.EligibleGrants {
				if g.Kind == types.GrantAPIKey && !g.RequiresApproval {
					return true
				}
			}
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
		Fix: "Point WARDYN_DEFAULT_POLICY at a composer-capable ceiling (e.g. examples/policies/composer-dev.json) and restart " +
			"wardynd. `make setup` now auto-picks it when a real model path is configured.",
	}, true
}

// claudeSubscriptionStagingCheck is the "will the per-run subscription mount
// actually work" readiness row. It fires ONLY when a resident Claude login is
// detected (no login => the llm_provider check already says "add one"). The gap
// it catches: the model-access badge reads green from the HOST login, but the
// per-run "Use my Claude subscription" mount only works after staging generates
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
			Detail: "Your Claude login is staged for sandbox use — the per-run \"Use my Claude subscription\" mount is available.",
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
			"sandbox use, so ticking \"Use my Claude subscription\" on a run won't work.",
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
			Fix: "Wire a multi-toolchain image via WARDYN_AGENT_IMAGES (e.g. build deploy/images/full (the fat toolchain image), or your " +
				"own image satisfying the IMAGE CONTRACT in deploy/images/README.md), or pass a per-run base image " +
				"in the New Run wizard's \"Custom sandbox image (advanced)\" field — Wardyn wraps it with the runner tools.",
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
//nolint:funlen,gocyclo,gocognit // Deliberate: one linear checklist that computes EVERY /setup/status readiness item in reading order (DB, runner, composer, LLM cred, workspaces, …); splitting it would scatter the checklist that operators and the wizard read as one unit. Next candidate if it grows: one helper per checklist item.
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

	// runner: copy handleHealthz's Capabilities pattern (nil Runner => "none",
	// empty classes). Driver is the runner's Name(), classes/substrates come from
	// the live Capabilities.
	rnr := SetupRunner{Driver: "none", ConfinementClasses: []string{}}
	if s.cfg.Runner != nil {
		rnr.Driver = s.cfg.Runner.Name()
		if c, err := s.cfg.Runner.Capabilities(ctx); err == nil {
			for _, cc := range c.ConfinementClasses {
				rnr.ConfinementClasses = append(rnr.ConfinementClasses, string(cc))
			}
			if len(c.Resolved) > 0 {
				rnr.ConfinementSubstrates = make(map[string]string, len(c.Resolved))
				for k, v := range c.Resolved {
					rnr.ConfinementSubstrates[string(k)] = v
				}
			}
		}
	}
	hasCC2Plus := false
	for _, cc := range rnr.ConfinementClasses {
		if cc != "CC1" {
			hasCC2Plus = true
			break
		}
	}

	// composer: enablement + boot-snapshot backends.
	comp := SetupComposer{Backends: s.cfg.ComposerBackends}
	if comp.Backends == nil {
		comp.Backends = []ComposerBackendReadiness{}
	}
	if s.cfg.Composer != nil && s.cfg.Composer.Enabled() {
		comp.Enabled = true
		comp.Default = s.cfg.Composer.Default()
	}

	// providers: resident coding-agent CLIs. Peek the resident Claude subscription
	// OAuth token (read-only; never refreshes) so we can label the claude row's
	// auth mode and enrich the LLM provenance with fresh/expired + inject wording.
	// Skipped when no subscription provider is wired (tests / unconfigured host).
	provs := setup.DetectCLIProviders()
	var subTok subscription.Token
	var subPeekErr error
	subWired := s.cfg.SubscriptionToken != nil
	if subWired {
		subTok, subPeekErr = s.cfg.SubscriptionToken.Peek()
	}
	subOK := subWired && subPeekErr == nil && subTok.Value != ""
	now := s.cfg.Now()

	providers := make([]SetupProvider, 0, len(provs))
	var claudeDetail string // subscription-aware detail for a logged-in claude CLI
	for _, p := range provs {
		sp := SetupProvider{
			Tool: p.Tool, Installed: p.Installed, LoggedIn: p.LoggedIn, LoginDetectedVia: p.LoginVia,
		}
		if p.Tool == "claude" && subWired {
			// auth_mode "subscription" iff a subscription OAuth token is present
			// (fresh OR expired — freshness is carried in the detail, not the mode).
			if subOK {
				sp.AuthMode = "subscription"
			}
			if p.LoggedIn {
				claudeDetail = subscriptionLLMDetail(subTok, subPeekErr, s.subscriptionInjectEnabled(), p.LoginVia, p.BinPath, now)
			}
		}
		providers = append(providers, sp)
	}

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
	// AWSMount mirrors resolveBedrockAuth's opt-in host-mode path: BedrockAWSConfigDir
	// set AND the dir still exists (stat it so a since-deleted ~/.aws doesn't read ready).
	awsMount := false
	if s.cfg.BedrockAWSConfigDir != "" {
		if st, err := os.Stat(s.cfg.BedrockAWSConfigDir); err == nil && st.IsDir() {
			awsMount = true
		}
	}
	bedrock := SetupBedrock{
		Region:        s.cfg.BedrockRegion,
		Model:         s.cfg.BedrockModel,
		CredsPresent:  present[bedrockAccessKeyIDSecret] && present[bedrockSecretAccessKeySecret],
		AWSMount:      awsMount,
		BearerPresent: present[bedrockAPIKeySecret],
	}
	bedrock.Ready = bedrock.ready()
	if llmDetail == "" && bedrock.ready() {
		llmDetail = fmt.Sprintf(
			"AWS Bedrock is configured (region %s, model %s); Claude runs authenticate via %s.",
			bedrock.Region, bedrock.Model, bedrock.credSourceDesc())
	}

	// Wardyn-managed subscription (container-login setup-token): a compose-mode
	// LLM-access source with no resident host login. Presence-only; folded in as an
	// ADDITIONAL winning signal so a managed-only operator reads "LLM access: ok"
	// without changing llmProvenance's own priority order or its tests.
	var harnessCreds []SetupHarness
	if blob, ok, berr := s.readManagedBlob(ctx, "anthropic"); berr == nil && ok {
		aging := s.cfg.Now().UTC().Sub(blob.CapturedAt) > harnessTokenAging
		harnessCreds = append(harnessCreds, SetupHarness{
			Provider: "anthropic", Captured: true,
			CapturedAt: blob.CapturedAt.Format(time.RFC3339), Aging: aging, SourceRunID: blob.SourceRunID,
		})
		if llmDetail == "" {
			llmDetail = "A Wardyn-managed Claude subscription token (captured via container login) is injected proxy-side into every run."
		}
	}
	// AWS SSO (containerized login) — reports TRUE expiry, not an age heuristic.
	// Not folded into llmDetail: it credentials Bedrock specifically, and the
	// bedrock_provider check already owns that story.
	if blob, ok, berr := s.readAWSSSOBlob(ctx); berr == nil && ok {
		now := s.cfg.Now().UTC()
		harnessCreds = append(harnessCreds, SetupHarness{
			Provider: awsSSOProvider, Captured: true,
			CapturedAt:  blob.CapturedAt.Format(time.RFC3339),
			ExpiresAt:   blob.ExpiresAt.Format(time.RFC3339),
			Expired:     blob.expired(now),
			Renewable:   blob.RefreshToken != "",
			SourceRunID: blob.SourceRunID,
		})
	}
	llmReady := llmDetail != ""

	// checks: the rows the wizard renders. "info" is used for permanent /
	// non-fixable or purely-optional conditions so the user is never shown a red
	// they cannot clear.
	checks := []SetupCheck{}

	switch {
	case rnr.Driver == "none" || len(rnr.ConfinementClasses) == 0:
		checks = append(checks, SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "fail",
			Detail: "No sandbox runner is configured, so runs cannot launch.",
			Fix:    "Start wardynd with -runner docker (built with -tags docker) so runs are confined and executed.",
		})
	case hasCC2Plus:
		labeled := make([]string, len(rnr.ConfinementClasses))
		for i, c := range rnr.ConfinementClasses {
			labeled[i] = tierLabel(types.ConfinementClass(c))
		}
		checks = append(checks, SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "ok",
			Detail: "Runner live with the Wall tier or stronger (" + strings.Join(labeled, ", ") + ").",
		})
	default:
		checks = append(checks, SetupCheck{
			ID: "runner", Label: "Sandbox runner", Status: "info",
			Detail: "Only the Fence tier (weakest — a shared-kernel container) is available on this host; runs work but with the lowest isolation.",
			Fix:    "Unlock the Wall or Vault tier: run `wardyn setup wall` (or `wardyn setup vault`) on the host — it detects your OS/Docker setup and prints the exact steps.",
		})
	}

	checks = append(checks, agentImageCheck(s.cfg.AgentImages))

	if llmReady {
		checks = append(checks, SetupCheck{
			ID: "llm_provider", Label: "LLM access", Status: "ok",
			Detail: llmDetail,
		})
	} else {
		// INFO, not a warning: a model/harness provider is OPTIONAL. It's only
		// needed to run an agent under Wardyn's own harness or to enable the AI Run
		// Composer — a plain governed run (bring-your-own-container / task_mode=exec,
		// or an interactive run you drive) needs no model. So "no model" is a
		// deliberate, non-blocking state, never a gap the operator must clear.
		checks = append(checks, SetupCheck{
			ID: "llm_provider", Label: "LLM access", Status: "info",
			Detail: "No model/harness provider configured (optional): needed only for agent-harness runs or the AI Run Composer. Bring-your-own-container and interactive runs work without one.",
			Fix:    "Optional — connect a Claude subscription/API key or Bedrock (Getting Started → Model), or bind creds to a workspace/container.",
		})
	}

	// bedrock_provider: surfaced only once the operator has touched ANY Bedrock
	// knob — silent otherwise, so the overwhelming majority who never use AWS
	// aren't shown an irrelevant row. ok when fully wired; warn when partially
	// configured (a real gap worth fixing); the fully-unconfigured case adds no
	// check at all.
	if bedrock.configured() {
		if bedrock.ready() {
			checks = append(checks, SetupCheck{
				ID: "bedrock_provider", Label: "AWS Bedrock", Status: "ok",
				Detail: fmt.Sprintf("Bedrock is configured (region %s, model %s) for Claude runs via %s.", bedrock.Region, bedrock.Model, bedrock.credSourceDesc()),
			})
		} else {
			var missing []string
			if bedrock.Region == "" {
				missing = append(missing, "-bedrock-region")
			}
			if bedrock.Model == "" {
				missing = append(missing, "-bedrock-model")
			}
			if !bedrock.CredsPresent && !bedrock.AWSMount && !bedrock.BearerPresent {
				missing = append(missing, "a credential — a read-only ~/.aws mount (-bedrock-aws-dir), a bedrock-api-key bearer secret, or aws-access-key-id + aws-secret-access-key secrets")
			}
			checks = append(checks, SetupCheck{
				ID: "bedrock_provider", Label: "AWS Bedrock", Status: "warn",
				Detail: "Bedrock is partially configured; runs will NOT use it until this is complete.",
				Fix:    "Still needed: " + strings.Join(missing, ", ") + ".",
			})
		}
	}

	// composer_llm_ceiling: a credential is present but does the DEFAULT POLICY
	// ceiling actually let a COMPOSED run use it? Catches the "everything green, first
	// run 404s" trap where WARDYN_DEFAULT_POLICY (e.g. demo.json/default.json) carries
	// no api_key grant. A resident Claude CLI login signals the subscription path.
	hasClaudeSub := false
	claudeLoginVia := ""
	for _, p := range providers {
		if p.Tool == "claude" && p.LoggedIn {
			hasClaudeSub = true
			claudeLoginVia = p.LoginDetectedVia
			break
		}
	}
	if chk, ok := composerCeilingCheck(s.cfg.DefaultPolicy, present["anthropic-api-key"], present["openai-api-key"], hasClaudeSub); ok {
		checks = append(checks, chk)
	}

	// claude_subscription_staging: the login is detected, but is it STAGED for the
	// per-run subscription mount? Catches the headless-`make setup` skip where the
	// badge is green yet the per-run checkbox silently does nothing.
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

	if comp.Enabled {
		checks = append(checks, SetupCheck{
			ID: "composer", Label: "AI Run Composer", Status: "ok",
			Detail: "The AI Run Composer is enabled (default backend: " + comp.Default + ").",
		})
	} else {
		checks = append(checks, SetupCheck{
			ID: "composer", Label: "AI Run Composer", Status: "info",
			Detail: "The AI Run Composer is not enabled (optional); runs can still be configured manually.",
			Fix:    "Set -composer-config / WARDYN_COMPOSER_CONFIG to enable natural-language run composition.",
		})
	}

	if s.cfg.AgeKeyDurable {
		checks = append(checks, SetupCheck{
			ID: "age_key", Label: "Secret store durability", Status: "ok",
			Detail: "The secret store age key is durable; stored secrets survive a restart.",
		})
	} else {
		checks = append(checks, SetupCheck{
			ID: "age_key", Label: "Secret store durability", Status: "warn",
			Detail: "The secret store uses an EPHEMERAL age key generated at boot; stored secrets (API keys, GitHub App credentials) become unreadable after a restart.",
			Fix:    "Generate a durable key with `wardynd -gen-age-key` and set it as WARDYN_AGE_KEY (or -age-key).",
		})
	}

	checks = append(checks, hostProxyCheck(hostProxy, plat.Containerized && !setup.HostProxySeeded()))

	// site_config: whether an operator-wide corporate baseline (upstream proxy,
	// artifact-registry overrides, default SCM hosts) has been authored yet.
	// Always "info" — it is optional and skippable, never a blocking gate.
	if s.cfg.Store != nil {
		if sc, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
			configured := sc.UpstreamProxySecretRef != "" || len(sc.ArtifactOverrides) > 0 || len(sc.ScmHosts) > 0
			if configured {
				checks = append(checks, SetupCheck{
					ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
					Detail: "An operator-wide site config is set (upstream proxy / artifact-registry overrides / SCM hosts); every run inherits it.",
				})
			} else {
				checks = append(checks, SetupCheck{
					ID: "site_config", Label: "Site config (corporate baseline)", Status: "info",
					Detail: "No operator-wide site config yet (optional): a corporate upstream proxy, artifact-registry redirects, and default SCM hosts that every run would inherit.",
					Fix:    "Set one via PUT /api/v1/site-config (or the Host Proxy / Artifact Redirect setup steps).",
				})
			}
			checks = append(checks, artifactRepoCheck(sc))
		}
	}

	checks = append(checks, scmProviderCheck(sec.GitHubApp, secretNames, scmPosture))

	// Platform info rows (permanent, non-fixable => "info").
	if plat.WSL {
		checks = append(checks, SetupCheck{
			ID: "platform_wsl", Label: "WSL networking", Status: "info", Platform: "wsl",
			Detail: "Running under WSL2: host<->sandbox networking is split. Reach the UI from Windows via localhost port-forwarding, and bind wardynd to a WSL-reachable address. With Docker Desktop's default NAT networking, sandbox->wardynd callbacks don't route in host mode — workspace Verify results never report and Record captures land empty.",
			Fix:    "Enable WSL2 mirrored networking ([wsl2] networkingMode=mirrored in %UserProfile%\\.wslconfig, then `wsl --shutdown`), or run the containerized stack (`make compose-up`) where callbacks route in-network.",
		})
	}
	if plat.OS == "darwin" {
		checks = append(checks, SetupCheck{
			ID: "platform_macos", Label: "macOS virtualization", Status: "info", Platform: "darwin",
			Detail: "macOS has no /dev/kvm; the Vault tier (CC3, hardware-virtualized) is unavailable — runs use container isolation.",
		})
	}

	// has_runs: cheap existence check via the store.
	// reuses ListRuns (fine for a first-run wizard); a dedicated
	// COUNT(*)/EXISTS is the upgrade if run volume ever makes this scan matter.
	hasRuns := false
	if s.cfg.Store != nil {
		if runs, err := s.cfg.Store.ListRuns(ctx); err == nil && len(runs) > 0 {
			hasRuns = true
		}
	}

	// ready: CONSERVATIVE — false when the runner is nil / has no live class, so
	// the wizard opens rather than hiding a half-configured bootstrap. Composer /
	// credentials are warnings, not readiness gates.
	ready := s.cfg.Runner != nil && len(rnr.ConfinementClasses) > 0

	writeJSON(w, http.StatusOK, SetupStatus{
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
	})
}

// scmProviderCheck grades the SCM credential posture against the safest-path
// ladder (GitHub App > fine-grained PAT > deploy key > classic PAT/gh token >
// personal SSH key). NEVER a gate: warn means "a safer option exists", not
// "broken" — the configured lane still clones fine, and public repos need no
// SCM credential at all. Grading:
//   - GitHub App configured        -> ok   (brokered ≤1h scoped tokens; the only
//     rung Wardyn itself can expire — nothing safer to recommend)
//   - any ssh-key-<host> secret    -> warn (a STANDING resident-lane key Wardyn
//     can neither scope nor expire; auto-used by every future SSH clone)
//   - git-pat-<host> only          -> info (could already be a fine-grained
//     rung-2 token — server-side we cannot tell it from a classic one, so no
//     warn; the detail carries the upgrade hint instead)
//   - nothing configured           -> info (+ posture-aware Fix when the host
//     shows loose habits: gh CLI login, credential.helper store/cache,
//     ~/.git-credentials, ~/.netrc)
//
// a secret-NAME prefix scan, not a grant-usage check (grants are
// per-run, not standing config) — the <host-slug> convention (dots→hyphens,
// e.g. git-pat-github-com) is the contract the ScmProviderStep UI follows.
func scmProviderCheck(githubApp bool, secretNames []string, posture setup.SCMPosture) SetupCheck {
	var pats, sshKeys []string
	for _, n := range secretNames {
		switch {
		case strings.HasPrefix(n, "git-pat-"):
			pats = append(pats, n)
		case strings.HasPrefix(n, "ssh-key-"):
			sshKeys = append(sshKeys, n)
		}
	}
	loosePosture := posture.GhCLI || posture.GitCredentialsFile || posture.Netrc ||
		strings.HasPrefix(posture.CredentialHelper, "store") || strings.HasPrefix(posture.CredentialHelper, "cache")
	switch {
	case githubApp:
		via := append([]string{"GitHub App"}, pats...)
		detail := "Safest lane configured: the GitHub App mints a brokered, ≤1h, contents-scoped token per run — the only SCM credential Wardyn itself can expire. (" + strings.Join(via, ", ") + ")"
		if len(sshKeys) > 0 {
			// Honesty: the App does NOT retire a standing ssh-key-* secret —
			// SSH-protocol clones still auto-use it, resident, with no prompt.
			detail += " Note: standing SSH key secret(s) also present (" + strings.Join(sshKeys, ", ") + ") — the App doesn't retire them; delete if unused."
		}
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "ok",
			Detail: detail,
		}
	case len(sshKeys) > 0:
		detail := "SSH key secret(s) configured (" + strings.Join(sshKeys, ", ") + "): a STANDING credential, resident in the sandbox for each clone, that Wardyn can neither scope nor expire. It works — a safer rung exists."
		if len(pats) > 0 {
			detail += " PAT secret(s) also present (" + strings.Join(pats, ", ") + "): those are brokered per-clone and never resident."
		}
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "warn",
			Detail: detail,
			Fix:    "Prefer a GitHub App (brokered, expirable) or a fine-grained repo-scoped PAT (github.com/settings/personal-access-tokens/new → Contents: Read-only). If SSH, make it a single-repo read-only deploy key, not a personal identity.",
		}
	case len(pats) > 0:
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "info",
			Detail: "PAT secret(s) configured (" + strings.Join(pats, ", ") + "), brokered per-clone and never resident. If it is a classic whole-account PAT, re-issue it fine-grained + repo-scoped + short-expiry; a GitHub App is safer still.",
		}
	case loosePosture:
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "info",
			Detail: "No SCM credential configured yet (optional). Host posture note: this machine keeps broad or plaintext git credentials (gh CLI session, credential.helper store/cache, ~/.git-credentials or ~/.netrc) — Wardyn never reads them.",
			Fix:    "For private repos, prefer a GitHub App or a fine-grained repo-scoped PAT stored as git-pat-<host-slug> (e.g. git-pat-github-com) — or generate a read-only deploy key (make setup offers this).",
		}
	default:
		return SetupCheck{
			ID: "scm_provider", Label: "SCM provider credentials", Status: "info",
			Detail: "No SCM credential configured yet (optional): cloning a private GitHub/Azure DevOps repo needs a GitHub App, a git-pat-<host-slug> secret (HTTPS/PAT), or an ssh-key-<host-slug> secret (SSH) referenced from a matching grant.",
			Fix:    "Add a secret named git-pat-github-com / git-pat-dev-azure-com (or your GHES/ADO-Server host's slug) under Secrets and reference it from a git_pat grant — or configure a GitHub App.",
		}
	}
}

// hostProxyCheck summarizes host-proxy detection as a single non-blocking
// "info" row — the HostProxy field itself carries the full per-source detail
// the Host Proxy step renders. Always "info": detection never blocks setup,
// it only surfaces what's already configured on the host so the step can
// suggest matching settings.
//
// blind is true when this wardynd is containerized AND no host-side detection
// was seeded in: every tier (shell profiles, git, tool configs, OS/PAC) is then
// structurally unreachable, so an empty result must say "couldn't look there"
// rather than assert "nothing is there". Same honesty rule as vaultKVMDetail.
func hostProxyCheck(d setup.HostProxyDetection, blind bool) SetupCheck {
	var found []string
	if d.HTTPProxy != nil || d.HTTPSProxy != nil || d.AllProxy != nil {
		found = append(found, "an env/shell/OS proxy setting")
	}
	if d.GitProxy != nil {
		found = append(found, "a git config proxy")
	}
	if len(d.ToolConfigs) > 0 {
		names := make([]string, len(d.ToolConfigs))
		for i, tc := range d.ToolConfigs {
			names[i] = tc.Tool
		}
		found = append(found, "tool configs ("+strings.Join(names, ", ")+")")
	}
	if d.PAC != nil {
		found = append(found, "a PAC/WPAD auto-config URL (cannot be resolved automatically)")
	}
	if len(found) == 0 {
		if blind {
			return SetupCheck{
				ID: "host_proxy", Label: "Host proxy", Status: "info",
				Detail: "Detection ran inside the wardynd container, so it only sees this container's environment — not your host's shell profiles, git config, per-tool configs, or OS/PAC proxy settings. That is \"couldn't look there\", not \"nothing is there\".",
				Fix:    "If your host uses a corporate proxy, store its URL as a secret and reference it below — or re-run `make setup` (it detects on the host and seeds the result in).",
			}
		}
		return SetupCheck{
			ID: "host_proxy", Label: "Host proxy", Status: "info",
			Detail: "No host-side proxy configuration detected (env vars, shell profiles, git config, tool configs, or OS proxy settings).",
		}
	}
	detail := "Detected " + strings.Join(found, "; ") + "."
	if d.HasCredentials {
		detail += " A detected proxy carries an embedded credential — store it as a secret rather than a plain URL."
	}
	return SetupCheck{ID: "host_proxy", Label: "Host proxy", Status: "info", Detail: detail}
}

// artifactRepoCheck reports whether the operator has configured artifact-registry
// redirects (Artifactory/Nexus mirrors per ecosystem). Always "info" — optional
// and non-blocking, mirroring the other new corporate-baseline steps; it just
// tells the operator which ecosystems every run now pulls from the corp mirror
// (and which of those inject a token proxy-side).
func artifactRepoCheck(sc types.SiteConfig) SetupCheck {
	if len(sc.ArtifactOverrides) == 0 {
		return SetupCheck{
			ID: "artifact_repo", Label: "Artifact repository redirection", Status: "info",
			Detail: "No artifact-registry redirects configured (optional): point npm/pip/cargo/maven/go/nuget at a corporate Artifactory/Nexus mirror so runs never reach public registries.",
			Fix:    "Set artifact_overrides via PUT /api/v1/site-config (or the Artifact Redirect setup step).",
		}
	}
	ecos := make([]string, 0, len(sc.ArtifactOverrides))
	tokened := 0
	for eco, ov := range sc.ArtifactOverrides {
		ecos = append(ecos, eco)
		if ov.TokenSecretRef != "" {
			tokened++
		}
	}
	sort.Strings(ecos)
	detail := "Redirecting " + strings.Join(ecos, ", ") + " to a corporate mirror; every run's egress substitutes the corp host for those public registries."
	if tokened > 0 {
		detail += fmt.Sprintf(" %d with a token injected proxy-side (the sandbox never holds it).", tokened)
	}
	return SetupCheck{ID: "artifact_repo", Label: "Artifact repository redirection", Status: "info", Detail: detail}
}
