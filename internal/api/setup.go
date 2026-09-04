// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/setup"
	"github.com/cjohnstoniv/wardyn/internal/store"
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
	// Providers reports resident coding-agent CLIs detected on the wardynd host.
	Providers []SetupProvider `json:"providers"`
	// Secrets reports which known secrets are present (NAMES only, reserved
	// names excluded) — never any value.
	Secrets SetupSecrets `json:"secrets"`
	// AgeKey reports whether the at-rest secret store survives a restart.
	AgeKey SetupAgeKey `json:"age_key"`
	// HasRuns drives the wizard's "launch your first run" done state.
	HasRuns bool `json:"has_runs"`
	// OnboardingComplete reports whether an operator has finished (or
	// deliberately left) the Getting Started funnel ON THIS INSTALL —
	// SiteConfig.OnboardingCompletedAt, flattened to the only bit the console
	// needs. It is a fact about the install, not about the browser: the console
	// used to keep this in localStorage, where it outlived wiped databases and
	// disagreed with itself between 127.0.0.1 and localhost (different origins,
	// different storage). The first-run landing, the welcome hero and the setup
	// gate all read THIS.
	OnboardingComplete bool `json:"onboarding_complete"`
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
	// LLMReady is the server-computed "does SOME run's LLM access path exist"
	// verdict (HIGH-4 review fix) — the same winning-signal logic that already
	// decides llmProvenance's detail (resident CLI login, a secret-name
	// heuristic, Bedrock, a managed harness token) OR'd with an AI-provider
	// Integration being configured. It exists
	// because a MEMBER'S redacted response (redactSetupStatusForMember) drops
	// the checks/providers/secret-name detail that would otherwise let the
	// console derive this itself — LLMReady is computed BEFORE redaction and
	// deliberately left untouched BY it, so the console's readiness chip / new-run
	// banner / demo gating keep working for a member without any of that detail
	// leaking. Kept in exact sync with ui/src/app/lib/types.ts's SetupStatus.
	LLMReady bool `json:"llm_ready"`
	// TrustedCACerts is the number of additional roots WARDYN_TRUSTED_CA_FILE
	// loaded at boot (0 = unset). Derived from Config.TrustedCAPEM, never a
	// second boot-time field — see handleSetupStatus. Go + test only: no
	// console reader exists yet (the ui/src/app/lib/types.ts mirror is
	// hand-maintained, added when the Network step renders it) and
	// redactSetupStatusForMember does not zero it — a bare count carries no
	// PEM content, host name, or other detail members are barred from.
	TrustedCACerts int `json:"trusted_ca_certs,omitempty"`
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
	// SharedSubscriptionAllowed reports whether this deployment may inject ONE
	// operator's Anthropic subscription into runs (single-user desktop only; see
	// subscriptionInjectPosture in cmd/wardynd). The console reads it to decide
	// whether to offer the "Connect Claude subscription" affordance at all —
	// rendering a sign-in that cannot work is worse than not offering it.
	SharedSubscriptionAllowed bool `json:"shared_subscription_allowed"`
	// SharedSubscriptionReason says WHY it is unavailable, so the UI can explain
	// rather than looking identical to "the operator never logged in". Empty when
	// allowed.
	SharedSubscriptionReason string `json:"shared_subscription_reason,omitempty"`
}

// SetupRunner echoes the runner name and the live confinement classes/substrates.
type SetupRunner struct {
	Driver                string            `json:"driver"`
	ConfinementClasses    []string          `json:"confinement_classes"`
	ConfinementSubstrates map[string]string `json:"confinement_substrates,omitempty"`
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
// for the WINNING signal (resident CLI login > api-key-ish secret) and "" when
// none is present — readiness is simply "llmProvenance != \"\"", so the boolean
// and the rendered detail can never drift.
//
// claudeDetail is the precomputed subscription-aware sentence for a resident
// Claude CLI login (see subscriptionLLMDetail); it is used only when a logged-in
// claude CLI is the winner, and falls back to a generic sentence when empty (the
// subscription provider was unwired, so no peek was possible).
func llmProvenance(providers []SetupProvider, secretNames []string, claudeDetail string) string {
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
// AI-provider Integration fallback for when llmDetail came up empty.
func computeLLMReady(llmDetail string, integrations []SetupIntegration) bool {
	if llmDetail != "" {
		return true
	}
	for _, in := range integrations {
		if types.AIProviderKind(in.Kind) {
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

// claudeSubscriptionStagingCheck is the "will a resident-host Claude
// subscription integration actually work" readiness row. It fires ONLY when a
// resident Claude login is detected (no login => the llm_provider check
// already says "add one"). The gap it catches: the model-access badge reads
// green from the HOST login, but a run only reaches it after staging generates
// the subscription ceiling (~/.wardyn/composer-dev-subscription.json) and
// wardynd restarts onto it — and `make setup` does not stage at all any more
// (scripts/stage-claude-creds.sh is the explicit, separately-gated path), so a
// logged-in host is unstaged by default. blessed mirrors run-host.sh's
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
				"a resident-host Claude subscription. Save the \"Claude subscription (resident host)\" connection under " +
				"Settings → Model provider (saving it makes it the agent-runs default), or pin it on a workspace's Model access.",
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
	if isConventionLimitedToolchainImage(ref) {
		// INFO, not warn. This is the SHIPPED DEFAULT: it is true of every stock
		// install, it is documented rather than misconfigured, and it clears only
		// by building or wiring a multi-toolchain image that a JS/Python operator
		// never needs. setup_checks.go reserves "info" for exactly that —
		// permanent or purely optional — and the first-run gate (ui setup-gate.ts)
		// redirects on warn, so grading this warn locked every stock install in
		// the funnel with no in-product way out.
		return SetupCheck{
			ID: "agent_image", Label: "Agent image toolchains", Status: "info",
			Detail: "The configured claude-code agent image (" + ref + ") is a shipped convention image with a " +
				"limited toolchain — a Go, Rust or Java workspace will fail verify/record with exit 127 " +
				"(toolchain not found).",
			Fix: "Wire a multi-toolchain image via WARDYN_AGENT_IMAGES (helm: env.WARDYN_AGENT_IMAGES) (e.g. build deploy/images/full " +
				"(the fat toolchain image), or your own image satisfying the IMAGE CONTRACT in deploy/images/README.md), or pass a " +
				"per-run base image in the New Run wizard's \"Sandbox image\" field — Wardyn wraps it with the runner tools.",
		}
	}
	// The setup connectivity probe (site_config_probe.go) dispatches the "base"
	// image, never "claude-code" (a probe is a bare curl task, not a coding
	// agent). Since 0.7 the claude-code catalog row's ImageKey ALSO points at
	// base, so on a stock deployment these are the same image and stating them
	// as a contrast would present one image as two. They diverge only when an
	// operator pins claude-code in WARDYN_AGENT_IMAGES — which is exactly when
	// an operator needs to know the probe does not follow that pin.
	probeRef := agentImage("base", images)
	detail := "claude-code harness image: " + ref + ". "
	if probeRef != ref {
		detail += "The setup connectivity probe runs the `base` image instead: " + probeRef + ". "
	}
	return SetupCheck{
		ID: "agent_image", Label: "Agent image toolchains", Status: "info",
		Detail: detail + "Wardyn cannot inspect image contents from the " +
			"control plane (no docker CLI in the distroless build) — verify a workspace to confirm its toolchains.",
	}
}

// isConventionLimitedToolchainImage reports whether ref is one of Wardyn's own
// shipped convention images — the ones known, by construction, to carry a
// limited toolchain, so a Go/Rust/Java workspace fails verify/record at exit 127.
//
// agent-base is in this set. It became reachable in 0.7 when the claude-code
// catalog row's ImageKey was re-pointed at `base` (agent-claude-code is not
// published), so the ghcr fallback now resolves here — and without this entry
// the check silently downgraded from warn to info for the DEFAULT install,
// which is exactly the configuration that most needs the warning. Verified
// against ghcr.io/cjohnstoniv/agent-base:0.6.4: node, npm, python3 and git are
// present; go, java and cargo are not.
//
// The pre-rename :demo tag stays matched so holdout boxes keep the accurate warn.
func isConventionLimitedToolchainImage(ref string) bool {
	// Prefix, not an exact tag: the ghcr convention carries the daemon's own
	// version tag (D19), not a fixed :latest — every published tag is the same
	// convention image.
	for _, p := range []string{
		"ghcr.io/cjohnstoniv/agent-claude-code:",
		"ghcr.io/cjohnstoniv/agent-base:",
	} {
		if strings.HasPrefix(ref, p) {
			return true
		}
	}
	switch ref {
	case "wardyn/agent-claude-code:local", "wardyn/agent-claude-code:demo", "wardyn/agent-base:local":
		return true
	}
	return false
}

// Host-proxy sweep memo. setup.DetectHostProxy's OS tier shells out to the
// platform's proxy configuration (registry/scutil/gsettings) and measured ~450ms
// per call on a WSL host — 90%+ of handleSetupStatus's cost, on an endpoint the
// console polls every 5s, so a single open Getting-started tab spent most of a
// core on re-reading a host setting that changes about never.
//
// Memoized here rather than on Server (the way githubRefRulesetCheck's cache is)
// on purpose: the answer is a property of the HOST, not of any one Server, so
// two Servers in one process would only duplicate the sweep. hostProxyDetect is
// the seam the memo test swaps; hostProxyCacheReset drops the memo (tests, and
// the Re-check path if one is ever wired to force a re-detect).
const hostProxyTTL = 30 * time.Second

var (
	hostProxyMu     sync.Mutex
	hostProxyAt     time.Time
	hostProxyVal    setup.HostProxyDetection
	hostProxyDetect = setup.DetectHostProxy
)

func cachedHostProxy() setup.HostProxyDetection {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	if !hostProxyAt.IsZero() && time.Since(hostProxyAt) < hostProxyTTL {
		return hostProxyVal
	}
	hostProxyVal = hostProxyDetect()
	hostProxyAt = time.Now()
	return hostProxyVal
}

// hostProxyCacheReset forgets the memo so the next caller re-detects.
func hostProxyCacheReset() {
	hostProxyMu.Lock()
	defer hostProxyMu.Unlock()
	hostProxyAt = time.Time{}
	hostProxyVal = setup.HostProxyDetection{}
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

	providers, claudeDetail := s.setupProviders()

	// secrets: names only (reserved excluded); github_app iff both App secrets present.
	secretNames, present, sec, err := s.setupSecretsSnapshot(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list secrets: "+err.Error())
		return
	}

	plat := setup.DetectPlatform()
	hostProxy := cachedHostProxy()
	scmPosture := setup.DetectSCMPosture()

	// LLM access provenance: the detail of the WINNING signal (resident CLI
	// login, or an api-key-ish secret), "" when none. The secret-name scan is a
	// loose substring signal; the exact truth (a working model call) is only
	// known at run time — this just decides whether to warn the operator up
	// front.
	llmDetail := llmProvenance(providers, secretNames, claudeDetail)

	// Bedrock readiness: region/model are boot-time config (non-secret, safe to
	// echo to the UI); CredsPresent mirrors resolveBedrockAuth's secret-name
	// check (presence, not the value). Folded into llmDetail as an ADDITIONAL
	// winning signal (not a change to llmProvenance's own priority order) so a
	// Bedrock-only operator still sees "LLM access: ok" without touching the
	// existing CLI/secret-name signals or their tests.
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
	// CLI login, a secret-name heuristic, Bedrock, or a managed harness token —
	// everything folded in above) OR'd with an
	// AI-provider Integration being configured, computed ONCE here and reused
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
	// confinement_floor: the operator's configured floor vs what this runner
	// can actually enforce — see confinementFloorCheck.
	if chk, ok := confinementFloorCheck(rnr, s.cfg.DefaultPolicy.MinConfinementClass); ok {
		checks = append(checks, chk)
	}
	// k8s_egress_containment: the boot-time NetworkPolicy canary verdict —
	// absent (no row) on a non-k8s driver; see k8sEgressContainmentCheck.
	if chk, ok := k8sEgressContainmentCheck(rnr.Driver, k8sNetpolProven); ok {
		checks = append(checks, chk)
	}
	if chk, ok := bedrockProviderCheck(bedrock); ok {
		checks = append(checks, chk)
	}

	hasClaudeSub, claudeLoginVia := claudeLoginSignal(providers)

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

	checks = append(checks, ageKeyCheck(s.cfg.AgeKeyDurable),
		hostProxyCheck(hostProxy, plat.Containerized && !setup.HostProxySeeded()))

	// sso_rbac / tls_cookie_posture: both OIDC-gated (mirror how every other
	// conditional check gates on its own applicability).
	oidcConfigured := s.cfg.OIDC != nil
	if chk, ok := ssoRBACCheck(oidcConfigured, s.cfg.OIDCRoleMapConfigured, s.consoleRoleMappingsPresent(ctx, oidcConfigured)); ok {
		checks = append(checks, chk)
	}
	if chk, ok := tlsCookiePostureCheck(oidcConfigured, s.cfg.OIDCRedirectURL, s.cfg.OIDCSecureCookies); ok {
		checks = append(checks, chk)
	}

	// Filled from the site-config read below, not a second one. A read failure
	// leaves it false, which is the conservative direction: it opens the funnel
	// rather than hiding it.
	onboardingComplete := false
	if s.cfg.Store != nil {
		if sc, err := s.cfg.Store.GetSiteConfig(ctx); err == nil {
			checks = append(checks, siteConfigCheck(sc, present), artifactRepoCheck(sc))
			onboardingComplete = sc.OnboardingCompletedAt != nil
		}
		// permissions_posture (#19b): non-blocking/informational, so a read
		// failure here is skipped rather than surfaced as a setup/status 500 —
		// unlike secrets/site-config above, nothing else on this page depends
		// on the enforcement map.
		if enf, err := s.cfg.Store.GetCapabilityEnforcement(ctx); err == nil {
			checks = append(checks, permissionsPostureCheck(enf))
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

	// has_runs: an EXISTENCE check, so it reads exactly one row. ListRuns builds
	// an unbounded `SELECT <every column> FROM agent_runs ORDER BY created_at
	// DESC` — every run this install ever launched, decoded in full, on an
	// endpoint the console polls every 5s — only to test len(runs) > 0. Use the
	// same Pager idiom firstBrokeredRepoFromRuns already uses
	// (setup_checks.go); ListRuns stays the fallback, which only test doubles
	// lacking Pager ever take (every real deployment is PG). A dedicated
	// COUNT(*)/EXISTS is the remaining upgrade, but LIMIT 1 already makes the
	// cost independent of run history.
	hasRuns := false
	if s.cfg.Store != nil {
		var runs []types.AgentRun
		var err error
		if pg, ok := s.cfg.Store.(store.Pager); ok {
			runs, err = pg.ListRunsPage(ctx, store.Page{Limit: 1})
		} else {
			runs, err = s.cfg.Store.ListRuns(ctx)
		}
		hasRuns = err == nil && len(runs) > 0
	}

	// ready: CONSERVATIVE — false when the runner is nil / has no live class, so
	// the wizard opens rather than hiding a half-configured bootstrap.
	// Credentials are warnings, not readiness gates.
	ready := s.cfg.Runner != nil && len(rnr.ConfinementClasses) > 0

	resp := SetupStatus{
		Ready:  ready,
		Checks: checks,
		Auth: SetupAuth{
			Mode: authMode, LocalLoopback: s.cfg.LocalLoopback,
			SharedSubscriptionAllowed: s.cfg.SubscriptionPostureOK,
			SharedSubscriptionReason:  s.cfg.SubscriptionPostureReason,
		},
		Runner:             rnr,
		Providers:          providers,
		Secrets:            sec,
		AgeKey:             SetupAgeKey{Durable: s.cfg.AgeKeyDurable},
		HasRuns:            hasRuns,
		OnboardingComplete: onboardingComplete,
		Platform:           SetupPlatform{OS: plat.OS, WSL: plat.WSL, KVM: plat.KVM},
		HostProxy:          hostProxy,
		SCM:                scmPosture,
		Bedrock:            bedrock,
		Deployment:         SetupDeployment{HostLike: deploymentHostLike(providers)},
		Harness:            harnessCreds,
		// Integrations reuses the single integrationsWithCapabilitiesUsing call
		// hoisted above (PLATFORM-API-7 optimization + HIGH-4 llm_ready reuse).
		Integrations: integrations,
		Harnesses:    setupHarnessTools(),
		LLMReady:     llmReady,
		// A count derived from the SAME PEM string TrustedCAPEM's doc comment
		// describes — no second boot-time field to keep in sync. 0 when unset.
		TrustedCACerts: strings.Count(s.cfg.TrustedCAPEM, "-----BEGIN CERTIFICATE-----"),
	}
	// DELIBERATELY isOperator (three-tier doctrine, internal/auth/oidc's
	// RoleSecurityAdmin): what this redaction drops is the DEPLOYER's funnel —
	// the environment/credential checklist, resident-CLI login detection,
	// secret names, runner detail — every row of it actionable only through a
	// setup mutation, which stays super-only. A security admin sees the same
	// summary a member does because there is nothing here they could act on.
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
// HasRuns, plus every field the run-launch UI reads (Bedrock, Deployment,
// Harness*, Integrations, Platform, HostProxy, SCM, AgeKey) so a member can
// still launch runs normally. This only ZEROES fields on an already-computed,
// already-200 response — it can never itself produce an error state (no
// non-401 error is possible for a member here, by construction).
//
// Runner.ConfinementClasses survives redaction: it is not diagnostic detail,
// it is the barrier-count signal ui/lib/readiness.ts's deriveReadiness reads
// verbatim to compute barrierReady, which gates the keyless demos' Start
// button (demo-screen.tsx) for every role. Dropping it zeroed barrierReady
// for every member regardless of the real runner state. Only Driver and the
// per-class ConfinementSubstrates map — genuine diagnostic detail — are
// dropped.
// consoleRoleMappingsPresent reports whether any console role-mapping rows
// exist, for ssoRBACCheck's merged-map presence input. It follows the SAME
// nil-Store guard the permissions_posture read applies: a nil Store or a
// failed read reports false, the conservative direction — it surfaces the
// sso_rbac warning rather than silently hiding it behind a People-step row
// this call could not actually confirm exists.
func (s *Server) consoleRoleMappingsPresent(ctx context.Context, oidcConfigured bool) bool {
	if !oidcConfigured || s.cfg.Store == nil {
		return false
	}
	rows, err := s.cfg.Store.ListRoleMappings(ctx)
	return err == nil && len(rows) > 0
}

func redactSetupStatusForMember(st SetupStatus) SetupStatus {
	st.Checks = []SetupCheck{}
	st.Providers = []SetupProvider{}
	st.Secrets = SetupSecrets{Present: []string{}}
	st.Runner = SetupRunner{ConfinementClasses: st.Runner.ConfinementClasses}
	// Host credential/environment posture — a description of the OPERATOR'S
	// MACHINE, not of anything a member can act on, and the last place a member
	// could read it off this endpoint. SCM names which git credentials sit on
	// the wardynd host's disk (a gh session, ~/.git-credentials, ~/.netrc, a
	// plaintext-ish "store"/"cache" helper); HostProxy carries the corporate
	// proxy topology, host:port and a "the operator's proxy credentials live
	// here" flag; Deployment.HostLike is derived from Providers, which is
	// redacted two lines up — keeping it published the resident-login signal
	// after dropping the detail that produced it.
	st.SCM = setup.SCMPosture{}
	st.HostProxy = setup.HostProxyDetection{}
	st.Deployment = SetupDeployment{}
	// Harness is REDUCED, not dropped: ui/lib/api/integrations.ts reads
	// provider/captured/expired to answer "is there a model path" for a
	// member's own readiness. Capture time, source run id, aging and
	// renewability are operator credential-lifecycle detail. Rebuilt into a new
	// slice rather than edited in place — the input is the caller's value.
	if len(st.Harness) > 0 {
		reduced := make([]SetupHarness, len(st.Harness))
		for i, h := range st.Harness {
			reduced[i] = SetupHarness{Provider: h.Provider, Captured: h.Captured, Expired: h.Expired}
		}
		st.Harness = reduced
	}
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
	return out, k8sNetpolVerdict(out.Driver, c)
}

// k8sNetpolVerdict grades a runner's aggregated NetworkPolicy signals into the
// three live-daemon verdicts, or "" on any non-k8s driver. Shared by
// setupRunnerInfo (admin-only /setup/status, its return value here is a k8s
// egress-containment checklist row) and handleHealthz (anonymous /healthz's
// "network_policy" field) — both already hold a driver name and a
// runner.Capabilities from a successful Capabilities() call; a driver whose
// Capabilities() itself errored never reaches this function, so that case
// grades "" the same way a non-k8s driver does, at the caller.
func k8sNetpolVerdict(driver string, caps runner.Capabilities) string {
	if driver != "k8s" {
		return ""
	}
	// caps.NetworkPolicy / caps.NetworkPolicyAcknowledged are the
	// orchestrator-aggregated ClassSupport signals; a genuinely indeterminate
	// canary (no ack, no override) refuses to boot entirely (internal/runner/
	// k8s's newWithClient), so a LIVE daemon can only ever report one of these
	// three. Acknowledged checked first: B1's WARDYN_K8S_ACK_AMBIENT_DEFAULT_DENY
	// produces a driver that never set NetworkPolicy true (it is not proof), so
	// the two are mutually exclusive in practice, but acknowledged-not-proven
	// must never read as the stronger "enforced" claim if that ever changed.
	switch {
	case caps.NetworkPolicyAcknowledged:
		return "acknowledged"
	case caps.NetworkPolicy:
		return "enforced"
	default:
		return "unenforced"
	}
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
