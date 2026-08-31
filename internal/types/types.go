// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package types defines Wardyn's core domain vocabulary: the four nouns
// (AgentRun, RunPolicy, CredentialGrant, ApprovalRequest) plus the audit
// event shape. These types are the single source of truth shared by the
// control plane, runners, sidecars, and (on Kubernetes) the CRD layer.
package types

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ConfinementClass declares how strongly a sandbox substrate can actually
// confine an agent. Policy may refuse high-trust credential scopes to weak
// classes; the UI must always display the class. See threatmodel/.
type ConfinementClass string

const (
	// CC1: hardened shared-kernel runc (userns, seccomp, AppArmor, cap-drop).
	CC1 ConfinementClass = "CC1"
	// CC2: gVisor userspace kernel (the default — needs a native Docker engine
	// with the runsc runtime registered; see `wardyn setup wall`).
	CC2 ConfinementClass = "CC2"
	// CC3: Kata microVM (requires /dev/kvm).
	CC3 ConfinementClass = "CC3"
)

// Rank orders Confinement Classes weakest→strongest (CC1<CC2<CC3). Unrecognised
// values rank 0 (below CC1) so they never satisfy a real minimum — callers that
// gate on a minimum class fail closed. Matching is exact; normalise the string
// first if the input is untrusted.
func (c ConfinementClass) Rank() int {
	switch c {
	case CC1:
		return 1
	case CC2:
		return 2
	case CC3:
		return 3
	default:
		return 0
	}
}

// ConfinementClassNames maps each wire code to the friendly display label the
// UI shows instead (ui/src/app/components/wardyn/cc-meta.ts's CC_META
// labels) — the Go-side source of truth for that mapping, so the CLI's
// --confinement alias parsing and the /healthz payload can't drift apart.
var ConfinementClassNames = map[ConfinementClass]string{
	CC1: "Fence",
	CC2: "Wall",
	CC3: "Vault",
}

// RunState is the AgentRun lifecycle state machine.
type RunState string

const (
	RunPending  RunState = "PENDING"
	RunStarting RunState = "STARTING"
	RunRunning  RunState = "RUNNING"
	// RunWaiting is a RESERVED, not-yet-produced state: no backend path
	// transitions a run into WAITING_FOR_CONFIRMATION today. It is the planned
	// human-in-the-loop gate — a run that pauses for an operator to confirm a
	// risky action mid-flight — and is deliberately kept wired end to end (UI
	// attention badge in App.tsx/runs.tsx, primitives.tsx label, SDK alias, e2e
	// fixtures) so the display + attention semantics are proven before the
	// producer lands with the approval-gated-run feature. Reserved, not dead:
	// removing it would strip a designed seam the UI already renders.
	RunWaiting  RunState = "WAITING_FOR_CONFIRMATION"
	RunStopped  RunState = "STOPPED"
	RunArchived RunState = "ARCHIVED"
	RunFailed   RunState = "FAILED"
	RunKilled   RunState = "KILLED"
	// RunCompleted is the terminal success state: the agent process exited 0.
	// A non-zero exit transitions the run to RunFailed instead. The completion
	// watcher (see internal/api/runs.go dispatch) sets this from RunRunning.
	RunCompleted RunState = "COMPLETED"
)

// IsTerminal reports whether the run has already ended — it can no longer be
// killed, stopped or dispatched. This is the SINGLE source of the terminal set:
// the API's terminal guards, the CLI's `run --wait` exit codes and the e2e polls
// all read it here (pkg/client aliases RunState, so SDK consumers get it too),
// because a hand-copied set drifts — omitting COMPLETED once already shipped a
// live Kill button on finished runs. The UI keeps the only other copy
// (ui/src/app/lib/types/runs.ts); TestTerminalRunStates_UIParity fails if the
// two ever disagree.
func (s RunState) IsTerminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunKilled, RunStopped, RunArchived:
		return true
	}
	return false
}

// ActorType distinguishes who performed an action in the audit stream.
// This is the attribution field the incumbents lack.
type ActorType string

const (
	ActorHuman  ActorType = "human"
	ActorAgent  ActorType = "agent"
	ActorSystem ActorType = "system"
)

// AgentRun is one governed execution of a coding agent on behalf of a human.
// Every run gets its own identity (SPIFFE ID), its own credential grants,
// and its own audit trail.
type AgentRun struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by"` // human principal (token `sub`)
	Agent     string    `json:"agent"`      // e.g. "claude-code", "codex-cli"
	Repo      string    `json:"repo"`       // e.g. "org/name"
	Task      string    `json:"task"`       // human task description
	// Title is the run's human NAME. Runs that share a title are grouped in the
	// console's run list. Empty for legacy rows and for system runs (scan,
	// harness login, workspace record/verify) — the console falls back to Task,
	// which is the only identity an untitled run has ever had.
	Title string `json:"title,omitempty"`
	// Description is optional free-text context: why this run exists. Never
	// interpreted by the control plane, only displayed.
	Description      string           `json:"description,omitempty"`
	PolicyID         *uuid.UUID       `json:"policy_id,omitempty"`
	ConfinementClass ConfinementClass `json:"confinement_class"`
	State            RunState         `json:"state"`
	SPIFFEID         string           `json:"spiffe_id"`     // spiffe://<trust-domain>/agent-run/<id>
	RunnerTarget     string           `json:"runner_target"` // "docker"
	SandboxRef       string           `json:"sandbox_ref,omitempty"`
	// Image is the RESOLVED sandbox image this run dispatched with (convention
	// image, devcontainer build, workspace-built, or BYOI-wrapped), persisted
	// for provenance. Written by a scoped update after image resolution; empty
	// for legacy rows and runs that never reached resolution.
	Image string `json:"image,omitempty"`
	// Interactive marks a run created for human-driven use: the sandbox is brought
	// up RUNNING but no agent task is exec'd and no completion watcher is started
	// (the human drives via `wardyn attach`). A non-interactive run execs the agent
	// with the task and is watched to completion. This is a first-class,
	// sandbox-determining choice — see internal/api dispatch.
	Interactive bool `json:"interactive"`
	// WorkspacePath is the primary host directory this run operates in (the first
	// local WorkspaceMount source resolved from its policy), denormalized here so
	// the control plane can DISCOURAGE — warn, never block — launching a second
	// independent agent against a host directory another active run already uses.
	// Empty for runs with no local host workspace (git-clone / ephemeral).
	WorkspacePath string `json:"workspace_path,omitempty"`
	// WorkspaceID, when set, marks this run as a governed SCAN run for that
	// onboarded workspace: the driver runs wardyn-scan after cloning instead of the
	// agent, and the scan-result endpoint persists the derived profile onto this
	// workspace from this TRUSTED linkage (not sandbox input). Nil for ordinary runs.
	WorkspaceID *uuid.UUID `json:"workspace_id,omitempty"`
	// WorkspaceIDs is the READ-ONLY denormalization of the onboarded workspaces
	// this run's RESOLVED policy spec referenced at create time
	// (referencedWorkspaces over spec.WorkspaceMounts + spec.WorkspaceRepos).
	// Set only by handleCreateRun (internal/api/runs.go); the four internal
	// step-run call sites (record/verify, scan, harness login, probe) leave it
	// nil. Distinct from WorkspaceID above, which stays the scan/verify/
	// record-only TRUSTED linkage a sandbox upload authorizes on: this column
	// grants nothing, is never sandbox input, and exists only to answer "which
	// workspace(s) does an `always` egress-approval decision persist to". Nil
	// for a run that references no onboarded workspace.
	WorkspaceIDs []uuid.UUID `json:"workspace_ids,omitempty"`
	// SourceID is the TRUSTED run→library-source linkage for a per-source scan
	// run (the three-tier retarget): the scan-facts upload authorizes on it the
	// way workspace runs authorize on WorkspaceID. Never set by user runs.
	SourceID *uuid.UUID `json:"source_id,omitempty"`
	// AutoStopAfterSec is the run's EFFECTIVE idle auto-stop cap, captured from the
	// resolved RunPolicySpec at creation (frozen for the run's life). The idle
	// reaper reads it from the run row so a run launched with an inline/default
	// policy — which has no stored policy_id to JOIN — is reaped like any other.
	// 0 = never auto-stop; <0 = explicitly never (interactive). See adapters.go.
	AutoStopAfterSec int `json:"auto_stop_after_sec,omitempty"`
	// AgentExecID is the docker exec id of the agent process for exec-based runs
	// (the default idle-container + `docker exec` path). Empty for exec-less /
	// main-process substrates (krun) and before Exec runs. Persisted so the crash
	// reconciler can observe AGENT liveness via ExecInspect across a wardynd
	// restart: for an idle-container run the container is `sleep infinity`, so
	// container liveness != agent liveness, and the exec id otherwise lived only in
	// the driver's in-memory map — lost on restart, stranding the run.
	AgentExecID string `json:"agent_exec_id,omitempty"`
	// FailureHint is an operator-facing one-line reason a run FAILED, stamped by
	// the dispatch/create failure paths (failAndRevoke). Precedent:
	// record.go RecordResult.FailureHint. Without it a run that dies BEFORE the
	// agent starts (an image that resolves control-plane-side but not on the
	// daemon, a lost sandbox ref, an opaque-LLM inspection refusal) is just a
	// reason-less FAILED badge — the real reason lived only in an audit row. Empty
	// for every run that did not fail this way (a clean FAILED-by-nonzero-exit
	// carries its exit code instead, and success/terminal-by-kill carry nothing).
	// Display-only; never interpreted by the control plane.
	FailureHint string `json:"failure_hint,omitempty"`
}

// SiteConfig is the operator-wide, admin-authored baseline every run inherits:
// a corporate upstream proxy, egress redirects (package-registry mirrors and,
// more generally, any outbound URL/host/IP redirect), and default SCM hosts.
// It is the ONE net-new persistence surface the enterprise Getting-Started
// enhancements introduce (the Host Proxy and Corporate Network / Egress
// Redirection steps read it; everything else rides secrets + grants). There is
// exactly one SiteConfig for the operator (a store singleton); GetSiteConfig
// returns the zero value when none has been written yet — "unconfigured" is a
// valid, common state, not an error.
//
// Secret VALUES never live here — only secret NAMES (refs) the broker/proxy
// resolve at dispatch/injection time, mirroring how RunPolicySpec's
// GrantSpec.Scope references secrets by name rather than embedding them.
type SiteConfig struct {
	// UpstreamProxySecretRef names a secret holding the corporate upstream proxy
	// URL (optionally with embedded user:pass), or "" when no upstream proxy is
	// configured. Mutually exclusive in PRACTICE with UpstreamProxyURL (either
	// may be set; UpstreamProxyURL wins when both are — see
	// resolveUpstreamProxyURL) but not rejected as a validation error, since an
	// operator migrating from one to the other may round-trip both briefly.
	UpstreamProxySecretRef string `json:"upstream_proxy_secret_ref,omitempty"`
	// UpstreamProxyURL is the corporate upstream proxy URL written IN THE CLEAR
	// (http only — see resolveUpstreamProxyURL). A proxy URL is topology, not a
	// credential, and forcing every operator through the write-only secret store
	// means a mistyped URL can never be read back to debug. It MUST NOT embed a
	// userinfo (user:pass@) — validateSiteConfig rejects that at write time with
	// a 400 telling the caller to use UpstreamProxySecretRef instead, which
	// exists precisely for a proxy that DOES need an embedded credential.
	UpstreamProxyURL string `json:"upstream_proxy_url,omitempty"`
	// ArtifactOverrides maps an ecosystem ("npm"|"pip"|"cargo"|"maven"|"go"|
	// "nuget") to its corporate artifact-registry redirect.
	//
	// Deprecated: superseded by EgressRedirects, which generalizes this from
	// package registries to any outbound URL/host. Kept ONLY so the PUT
	// /site-config request decoder and `wardyn site-config apply` keep accepting
	// a document saved before this release — decodeStrict rejects unknown JSON
	// fields, so removing this field would turn every such legacy body into a
	// hard 400 instead of a fold. handlePutSiteConfig folds a non-empty value
	// into EgressRedirects (rejecting a body that sets both) and never persists
	// this field again; migration 0030 performs the same rewrite once, in place,
	// on the one already-stored document. Never populated by a read from
	// storage post-migration — treat a non-empty value outside the fold as
	// legacy request input only.
	ArtifactOverrides map[string]ArtifactOverride `json:"artifact_overrides,omitempty"`
	// EgressRedirects is the operator's outbound redirect list: FROM a public/
	// upstream URL or host, TO a corporate-internal replacement, generalizing
	// ArtifactOverride from package registries to any destination (a container
	// registry, a telemetry/SDK callback host, ...). Each entry is one of two
	// tiers, discriminated by Ecosystem:
	//
	//   - Ecosystem set (one of the ArtifactOverride closed set): FULL behavior,
	//     unchanged from the old ArtifactOverride — a per-tool config file
	//     (.npmrc/pip.conf/.cargo/config.toml/.m2/settings.xml/NuGet.Config/
	//     GOPROXY+GOSUMDB) via workspacescan.EmitArtifactConfig, PLUS egress
	//     substitution, PLUS token injection.
	//   - Ecosystem "" (NETWORK-ONLY): egress substitution (To's host allowed,
	//     From's host dropped) PLUS token injection for To's host, but NO config
	//     file — there is no ".npmrc equivalent" for an arbitrary host (a
	//     container registry, a telemetry endpoint, ...), and inventing one
	//     would be a lie about what Wardyn actually configures.
	EgressRedirects []EgressRedirect `json:"egress_redirects,omitempty"`
	// ScmHosts are the operator's default SCM hosts (e.g. "dev.azure.com",
	// "github.example.com") the SCM Provider step / egress bundling consult.
	ScmHosts []string `json:"scm_hosts,omitempty"`
	// Integrations are the operator-configured external connections (AI
	// providers, SCM hosts, generic connections) — the generalized replacement
	// UpstreamProxySecretRef/EgressRedirects are migrating toward. Both the
	// legacy fields and this one are read; nothing here removes the legacy
	// fields yet. IntegrationList folds pre-base-component rows forward at
	// decode and drops legacy artifact_mirror/host_proxy topology rows.
	Integrations IntegrationList `json:"integrations,omitempty"`
	// InternalHosts declares an internal hostname (an in-cluster service, a
	// corporate registry, the internal model gateway) the proxy's unconditional
	// private/reserved-IP SSRF guard would otherwise refuse regardless of
	// policy. Each entry LIFTS that guard for addresses matching its
	// host_suffix, scoped to its own CIDRs (or the full RFC1918/ULA/CGNAT set
	// when CIDRs is empty) — never loopback/link-local/metadata/unspecified/
	// multicast/NAT64, which stay denied unconditionally. The policy verdict
	// (allowed_domains/denied_domains) still has to allow the host separately —
	// this only lifts the SSRF builtin. Admin-only; validated at write time
	// (validateInternalHosts) so every declared CIDR lies inside
	// ipguard.Liftable. Empty (the default) => no lift, byte-identical to today.
	InternalHosts []InternalHost `json:"internal_hosts,omitempty"`
	// OnboardingCompletedAt records when an operator finished (or deliberately
	// left) the Getting Started funnel on THIS INSTALL. Nil until then.
	//
	// It lives here, server-side, because it is a fact about the install and
	// every previous attempt to keep it in the browser was wrong in a way that
	// shipped: a localStorage flag outlives the install it describes (a wiped
	// database kept skipping its own funnel) and is scoped to an ORIGIN, so the
	// same console reached at 127.0.0.1 and at localhost disagreed about
	// whether onboarding had happened.
	//
	// NOT settable through PUT /site-config: the handler rejects a client-supplied
	// value and carries the stored one forward, exactly as it does for
	// Integrations — otherwise a naive GET-then-PUT round-trip by an older
	// client would silently erase it.
	OnboardingCompletedAt *time.Time `json:"onboarding_completed_at,omitempty"`
}

// InternalHost is one SiteConfig.InternalHosts entry — see that field's doc.
type InternalHost struct {
	// HostSuffix matches a request host by label suffix: HostSuffix itself, or
	// any host ending in "."+HostSuffix (never a substring/mid-label match).
	HostSuffix string `json:"host_suffix"`
	// CIDRs scopes the lift to these ranges only. Each must lie entirely inside
	// RFC1918, fc00::/7, or 100.64.0.0/10 (ipguard.Liftable) — never loopback/
	// link-local/metadata/multicast/NAT64. Empty means the lift applies to the
	// full Liftable set for a matching host.
	CIDRs []string `json:"cidrs,omitempty"`
}

// ArtifactOverride is one ecosystem's corporate artifact-registry redirect: the
// base URL to emit into that ecosystem's config (.npmrc/pip.conf/cargo config/
// settings.xml/GOPROXY/nuget.config) plus an optional secret ref for a token
// injected proxy-side (the sandbox never holds the value).
//
// Deprecated: superseded by EgressRedirect (BaseURL -> To, unchanged
// semantics). See SiteConfig.ArtifactOverrides for why the type is kept.
type ArtifactOverride struct {
	BaseURL        string `json:"base_url"`
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
}

// EgressRedirect is one outbound redirect: requests to From are substituted to
// To (From's host dropped from egress, To's host allowed), with an optional
// token injected proxy-side for To's host. See SiteConfig.EgressRedirects for
// the two-tier Ecosystem behavior. From/To are validated (validateSiteConfig)
// with the same control-char/shell-metacharacter/real-host discipline as the
// legacy ArtifactOverride.BaseURL — either a full http(s) URL (validSiteURL) or
// a bare host (validSiteHost); workspacescan.EmitArtifactConfig relies on that
// safety for its raw string interpolation into .npmrc/settings.xml/etc, so
// keep any future validation change there in sync.
type EgressRedirect struct {
	// From is the public/upstream URL or host being redirected away from.
	From string `json:"from"`
	// To is the corporate-internal URL or host every matching run's egress is
	// substituted to. For an Ecosystem row this is also the value emitted into
	// that ecosystem's config file (the old ArtifactOverride.BaseURL).
	To string `json:"to"`
	// TokenSecretRef optionally names a secret whose value is injected
	// proxy-side as a Bearer token for To's host (the sandbox never holds it).
	// Mutually exclusive with TokenIntegrationRef — validateSiteConfig rejects a
	// row that sets both, since they answer the same question two ways.
	TokenSecretRef string `json:"token_secret_ref,omitempty"`
	// TokenIntegrationRef optionally names an Integration (SiteConfig.
	// Integrations[i].ID) to take this redirect's token FROM, instead of naming
	// a bare secret in TokenSecretRef.
	//
	// This is the seam between the two surfaces, and it exists because a private
	// registry is genuinely both things: a SYSTEM you authenticate to, and
	// sometimes the DESTINATION a public endpoint is rerouted to. The rule that
	// keeps them from duplicating each other — the integration owns the system
	// and its credential; the redirect owns rerouting a public endpoint to it.
	// So a redirect points AT the integration rather than restating its secret.
	//
	// It carries more than the secret name: the integration's Header and Format
	// come with it, so a feed that authenticates with something other than
	// "Authorization: Bearer" finally can (the bare-secret path below is
	// hardcoded to that shape). A ref naming nothing, a disabled row, or one with
	// no header credential degrades to redirect-WITHOUT-token, exactly as a
	// dangling TokenSecretRef already does — a redirect that still reroutes is
	// more useful than a run that fails.
	TokenIntegrationRef string `json:"token_integration_ref,omitempty"`
	// Ecosystem, when set, is one of the six package-manager ecosystems
	// ("npm"|"pip"|"cargo"|"maven"|"go"|"nuget") this redirect ALSO emits a
	// per-tool config file for, in addition to the egress substitution and
	// token injection every redirect gets. Empty means NETWORK-ONLY: no config
	// file is emitted (see SiteConfig.EgressRedirects).
	Ecosystem string `json:"ecosystem,omitempty"`
}

// GrantKind enumerates broker-mintable credential kinds.
type GrantKind string

const (
	GrantGitHubToken GrantKind = "github_token"
	GrantCloudSTS    GrantKind = "cloud_sts" // HARD-REQUIRES SPIRE identity provider
	GrantAPIKey      GrantKind = "api_key"   // proxy-side injection only
	// GrantGitPAT returns a STORED Personal Access Token VALUE to the git
	// credential helper (username/password) for a matched non-GitHub git host
	// (Azure DevOps / GitLab). This is the OPPOSITE of api_key: git-over-HTTPS to
	// those hosts is an opaque CONNECT tunnel the proxy cannot inject Basic-auth
	// into, so the credential must reach git via the helper — like github_token.
	GrantGitPAT GrantKind = "git_pat"
	// GrantSSHKey materializes a RESIDENT, agent-readable SSH private key so a run
	// can clone git-over-SSH. It is a DOCUMENTED EXCEPTION to the no-resident-
	// secret invariant: git's SSH transport has NO credential-helper seam (git
	// credential.helper is HTTP-only), so neither the git_pat helper trick nor
	// api_key proxy-side injection can carry the key — it MUST land as a 0400 file
	// the ssh client reads. agent-run writes it just before the clone and wipes it
	// right after (see deploy/images/*/agent-run), so the readable window is the
	// clone only. Transport is SSH-over-443 (ssh.github.com / ssh.dev.azure.com)
	// through the wardyn-proxy CONNECT tunnel — no port-22 egress. Residual risk is
	// the same posture as WARDYN_GIT_HELPER_SECRET: code running AS the agent uid
	// can read the key during that window. See broker.mintSSHKey + threat model.
	GrantSSHKey GrantKind = "ssh_key"
	// GrantEnvSecret places a STORED SECRET's value into the sandbox environment
	// under an operator-named variable, at dispatch. It is the second DOCUMENTED
	// EXCEPTION to the no-resident-secret invariant, and the honest reason is
	// coverage, not impossibility: a PAT-authenticated CLI or REST tool reads a
	// *_TOKEN env var, and neither the git_pat helper seam (git-only) nor api_key
	// proxy-side injection (one host, one header) can reach it. See
	// docs/adoption/corp-network-onboarding-findings.md B1.
	//
	// It is UNLIKE every other kind in three ways an operator must weigh:
	//
	//   - NOT BROKERED. There is no mint, no approval gate, no TTL and no JTI —
	//     the value is resolved store->env at dispatch (api.resolveEnvSecretGrants,
	//     the same seam resolveLLMInspectionSecrets uses) and the broker refuses
	//     the kind outright (mintKind's ErrUnknownGrantKind).
	//   - RESIDENT FOR THE WHOLE RUN. Unlike ssh_key's clone window, an env var
	//     lives as long as the process tree does; anything running as the agent
	//     uid can read /proc/self/environ.
	//   - NO REVOCATION. The kill-switch cascade revokes minted credentials; a
	//     value already in a process env is not one, so killing the run stops the
	//     process but does not un-disclose the secret.
	//
	// It is mask-registered at dispatch, and ADMIN-ONLY by default: a member's
	// env_secret grant is dropped unless the operator opens
	// WARDYN_ALLOW_MEMBER_ENV_SECRET. Scope is {"name":"MY_TOKEN",
	// "secret_name":"stored-name"}. See threatmodel/THREAT-MODEL.md §5.1a.
	GrantEnvSecret GrantKind = "env_secret"
)

// SubscriptionOAuthSecret is a SENTINEL secret name (NOT a stored secret). An
// api_key injection grant carrying it resolves at inject time to the operator's
// LIVE Anthropic subscription OAuth token (from the resident ~/.claude), not a
// value in the secret store — so subscription runs are credentialed proxy-side
// like api-key runs, the sandbox holding only the inert sentinel. It also serves
// as the durable "this profile uses subscription LLM auth" marker on a recorded
// profile (the resident ~/.claude mount that would otherwise signal subscription
// is never synthesized). Shared here so api + recordmode + UI agree on the name.
const SubscriptionOAuthSecret = "anthropic-subscription-oauth"

// ManagedOAuthSecret is a SENTINEL secret name (NOT a stored secret) that works
// exactly like SubscriptionOAuthSecret at the injection sink (host-pinned to
// api.anthropic.com, forced Authorization: Bearer, value masked), but resolves
// to the Wardyn-MANAGED subscription token: a long-lived `claude setup-token`
// OAuth token the operator captured via the container-login flow and Wardyn
// persisted (see internal/api/harnesscred.go). This is what lets a COMPOSE/
// containerized deployment — whose distroless wardynd has no host ~/.claude to
// read — credential a subscription run proxy-side without ever making the token
// resident in the sandbox. Distinct from SubscriptionOAuthSecret only in its
// SOURCE (managed store vs resident host ~/.claude), so the audit trail names
// which one credentialed a run.
const ManagedOAuthSecret = "anthropic-managed-oauth"

// GrantSpec is a credential scope description. The broker enforces the
// invariant: a minted credential's scope is exactly the approved scope —
// never wider (no scope-widening between request and mint).
type GrantSpec struct {
	Kind GrantKind `json:"kind"`
	// Scope is kind-specific: for github_token {"repos":[...],"permissions":{...}},
	// for api_key {"host":"...","header":"..."}, for git_pat
	// {"host":"...","secret_name":"...","username":"<optional>"} — the stored
	// secret_name's PAT value is returned to the git credential helper as the
	// password for host, with username resolved by convention (ADO=pat,
	// GitLab=oauth2) unless overridden — and for ssh_key
	// {"host":"...","key_secret_ref":"...","username":"<optional, default git>",
	// "known_hosts_secret_ref":"<optional>"} — the stored key_secret_ref's private
	// key VALUE is returned to agent-run, which writes it to a 0400 file for the
	// SSH-over-443 clone and wipes it after (see GrantSSHKey).
	Scope json.RawMessage `json:"scope"`
	// TTL of the minted credential. Max (and default) 1h.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// RequiresApproval forces a human approval to mint (vs auto-mint on policy).
	RequiresApproval bool `json:"requires_approval"`
}

// CredentialGrant records what a run is ELIGIBLE for. Eligibility is not
// issuance: minting happens only via the broker, and for RequiresApproval
// grants only inside the same DB transaction that verifies an APPROVED
// ApprovalRequest for this run+scope.
type CredentialGrant struct {
	ID        uuid.UUID `json:"id"`
	RunID     uuid.UUID `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
	Spec      GrantSpec `json:"spec"`
}

// ResolvedInjection is the ONE wire contract of GET
// /api/v1/internal/injection/{grantID}: the control plane's injection-resolve
// result, carrying the header name and the FORMATTED secret value (formatting
// applied server-side). ExpiresAt (unix ms, 0 = never) marks a rotating
// credential the proxy must re-resolve before it lapses (the subscription OAuth
// token); a static api-key grant leaves it 0.
//
// It lives here, in the neutral package both sides already import, because the
// api server (encoder) and the wardyn-proxy (decoder) previously each kept a
// hand-copied struct. Nothing pinned the two together, so a one-sided rename of
// expires_at would have silently made the proxy read 0 = "static credential,
// never re-resolve" and let a live OAuth token lapse mid-run. Sharing the type
// makes the compiler, not a parity test, the thing that keeps them equal.
type ResolvedInjection struct {
	Host      string `json:"host"`
	Header    string `json:"header"`
	Value     string `json:"value"`
	JTI       string `json:"jti"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
}

// ApprovalKind enumerates what a human is being asked to approve.
type ApprovalKind string

const (
	ApprovalCredential   ApprovalKind = "credential"
	ApprovalEgressDomain ApprovalKind = "egress_domain"
	ApprovalToolCall     ApprovalKind = "tool_call"
)

// ApprovalState is the approval lifecycle.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "PENDING"
	ApprovalApproved ApprovalState = "APPROVED"
	ApprovalDenied   ApprovalState = "DENIED"
	ApprovalExpired  ApprovalState = "EXPIRED"
)

// The approval sentinels live here, in the one package both internal/store and
// internal/approval already import, so the FSM can errors.Is a store error
// instead of matching its message text (which it used to do, silently breaking
// the moment either message was reworded or wrapped). store.ErrAlreadyDecided /
// approval.ErrAlreadyDecided and store.ErrDuplicatePending are aliases of these.
var (
	// ErrApprovalAlreadyDecided: a decision was attempted on an approval that
	// has already left PENDING. Fail closed — never let a second decision
	// silently overwrite the first.
	ErrApprovalAlreadyDecided = errors.New("approval already decided")
	// ErrDuplicatePendingApproval: a partial unique index rejected a second open
	// PENDING approval for the same dedup key, i.e. a concurrent raise lost the
	// race. It is a dedup signal (re-read the winner), NOT a hard failure.
	ErrDuplicatePendingApproval = errors.New("duplicate pending approval")
)

// ApprovalScope is how far a human's approve/deny decision reaches. It is
// ORTHOGONAL to FirstUseMode: that policy setting decides whether an unknown
// host is escalated to a human at all; this decides the blast radius of the
// answer. egress_domain approvals carry any of the four; a tool_call carries
// none (the clamp bounds it); a CREDENTIAL approval carries ScopeRun and
// nothing else — that one value is the per-run credential lease (B2), read RAW
// by broker.leaseCoversRemint so a git_pat approved once is re-mintable for the
// rest of the run instead of raising a fresh approval per git operation.
type ApprovalScope string

const (
	// ScopeOnce releases exactly one connection. For HTTPS that is one CONNECT
	// tunnel (which may carry many requests); for plain HTTP the proxy
	// re-evaluates per request, so it really is one request there.
	ScopeOnce ApprovalScope = "once"
	// ScopeRun holds for the rest of the run. This is the legacy meaning of
	// every decision made before scopes existed, and stays the default.
	ScopeRun ApprovalScope = "run"
	// ScopeUntil holds for the rest of the run OR until DecisionExpiresAt,
	// whichever comes first. Requires DecisionExpiresAt.
	ScopeUntil ApprovalScope = "until"
	// ScopeAlways additionally persists the host onto the run's workspace
	// (approved_egress / denied_egress) so future runs inherit it. Operator-only.
	ScopeAlways ApprovalScope = "always"
)

// Valid reports whether s is empty (unset => legacy run-scoped) or one of the
// four known scopes. Used to reject a garbage value at the API write boundary,
// mirroring FirstUseMode.Valid; runtime reads still fail closed via Normalize.
func (s ApprovalScope) Valid() bool {
	switch s {
	case "", ScopeOnce, ScopeRun, ScopeUntil, ScopeAlways:
		return true
	default:
		return false
	}
}

// Normalize resolves a stored/wire value for every runtime read, and it treats
// its two "not a known scope" cases DIFFERENTLY on purpose:
//
//   - EMPTY means "an old client, or a row written before this column existed".
//     Those already meant run-scoped, so widening them to anything else would
//     silently change shipped behavior. Empty => ScopeRun.
//   - UNKNOWN NON-EMPTY can only come from a NEWER control plane writing a scope
//     this binary does not understand (Valid() rejects garbage at the boundary).
//     Treating that as ScopeRun would be fail-OPEN across versions, so it floors
//     to the tightest scope instead. Unknown => ScopeOnce.
//
// Honest caveat: "tightest" is allow-shaped. On a DENY, once is the WIDER
// choice (it re-raises; run stays denied), so an unknown scope from the future
// turns a cached deny into a re-raise. That fails closed at the proxy — the
// re-raise is denied again under deny_with_review — but it does cost queue
// noise, so this is not uniformly fail-closed and should not be described as if
// it were.
func (s ApprovalScope) Normalize() ApprovalScope {
	switch s {
	case ScopeOnce, ScopeRun, ScopeUntil, ScopeAlways:
		return s
	case "":
		return ScopeRun
	default:
		return ScopeOnce
	}
}

// ApprovalRequest is a blocking human-in-the-loop gate. RequestedScope is
// EXACTLY what the approver saw; the broker writes MintedJTI back in the
// same transaction as the mint, yielding the provable join
// "approval X by human Y minted credential Z".
type ApprovalRequest struct {
	ID             uuid.UUID       `json:"id"`
	RunID          uuid.UUID       `json:"run_id"`
	GrantID        *uuid.UUID      `json:"grant_id,omitempty"`
	Kind           ApprovalKind    `json:"kind"`
	RequestedScope json.RawMessage `json:"requested_scope"`
	State          ApprovalState   `json:"state"`
	RequestedAt    time.Time       `json:"requested_at"`
	DecidedAt      *time.Time      `json:"decided_at,omitempty"`
	DecidedBy      string          `json:"decided_by,omitempty"`
	MintedJTI      string          `json:"minted_jti,omitempty"`
	Reason         string          `json:"reason,omitempty"`
	// DecisionScope is how far the human's decision reaches: once (one
	// connection), run (rest of this run — the default and the legacy meaning),
	// until (this run, until DecisionExpiresAt), or always (persisted onto the
	// workspace so future runs inherit it). Empty on a PENDING row — a decision
	// nobody has made yet has no scope — which is why the column is
	// NOT NULL DEFAULT '' rather than DEFAULT 'run'.
	//
	// Named decision_scope, NOT scope: RequestedScope above is the unrelated
	// host JSON, and it is part of the PENDING dedup unique index.
	DecisionScope ApprovalScope `json:"decision_scope,omitempty"`
	// DecisionExpiresAt is set only when DecisionScope is until. Nullable, so it
	// scans into a pointer the way DecidedAt does. NOT the same clock as the
	// EXPIRED state / approval.expire sweeper, which ages out stale PENDING
	// requests — this bounds a GRANT that was actually made.
	DecisionExpiresAt *time.Time `json:"decision_expires_at,omitempty"`
}

// ApprovalDecision is what a human — or the sweeper, for a stale-PENDING
// expiry — is deciding: which state the approval moves to, who decided and
// why, and (egress_domain approvals only) how far the decision reaches. It
// is threaded as ONE value through DecideApproval and ApprovalService.Decide
// rather than growing those signatures to five-plus positional parameters.
//
// ExpireStale is the one caller that leaves Scope at its zero value (""),
// never ScopeRun: an expiry is a sweep nobody decided, and ApprovalScope's
// own doc already explains why an unmade decision must never be asserted as
// "run"-scoped.
type ApprovalDecision struct {
	State     ApprovalState
	DecidedBy string
	Reason    string
	// Scope and ExpiresAt are meaningful only for an egress_domain approval;
	// every other kind leaves them at the zero value. See ApprovalScope.
	Scope     ApprovalScope
	ExpiresAt *time.Time
}

// AuditEvent is one append-only audit record. Every credential mint/revoke,
// approval decision, policy change, egress decision, and lifecycle change
// emits one. Events carry the delegation chain (human sub + agent run).
//
// Action namespaces (the dotted-verb prefix discriminates the source stream):
//
//	credential.*  identity.*  approval.*  policy.*  egress.*  run.* recording.*
//	    — control-plane / agent self-report events (Postgres event log + PTY).
//	kernel.*      — the eBPF/Tetragon GROUND-TRUTH stream (the tamper-proof
//	                second stream). Emitted ONLY by the host-scoped sensor
//	                (cmd/wardyn-tetragon-ingest) via POST /api/v1/internal/
//	                groundtruth, which FORCES actor_type=system +
//	                actor="wardyn-tetragon-ingest" and rejects any action that
//	                does not carry the "kernel." prefix. The defined kernel.*
//	                actions are:
//	                  kernel.process.exec    — observed execve
//	                  kernel.network.connect — observed outbound TCP connect
//	                  kernel.file.write      — observed write to a sensitive path
//	                  kernel.sensor.heartbeat— sensor liveness (run_id NULL)
//	                  kernel.sensor.blind    — host eBPF blind to a run (CC3/Kata)
//
// Data shape for the kernel.* (ebpf) stream. audit_events.data is JSONB, so
// this requires NO schema change — it is a documented convention over the
// existing column. Every kernel.* event carries:
//
//	{
//	  "stream": "ebpf",                 // discriminates from agent self-report
//	  "subtype": "process_exec" | "network_connect" | "file_write" | ...,
//	  "cgroup_id": <uint64>,            // kernel cgroup id (omitempty)
//	  "container_id": "<id>",           // attributed container (omitempty)
//	  "argv": [...] | "dst": "ip:port" | "path": "/...",  // kind-specific
//	  "loader": true,                   // exec of a dynamic linker (ld-linux/
//	                                    // ld-musl) — the documented ld-linux/
//	                                    // mmap bypass surface, FLAGGED not blocked
//	  "correlation": "mapped" | "unmapped",  // unmapped => run_id NULL, never
//	                                          // silently dropped (visible blindness)
//	  "reason": "...",                  // sensor.blind / failure detail (omitempty)
//	  "dropped_total": <uint64>,        // heartbeat only: sensor backpressure drops
//	  "observed_total": <uint64>,       // heartbeat only: kernel events mapped off the tail
//	  "dropped_unmapped": <uint64>      // heartbeat only: events dropped as
//	                                    // uncorrelated — nonzero with
//	                                    // observed_total 0 means correlation is
//	                                    // broken, NOT that the sensor is blind
//	}
//
// Outcome stays within the existing CHECK ("success"|"failure"|"denied"): the
// kernel stream uses "success" for normal observations and "failure" for the
// unexpected — an unmapped/escape signal such as a connect to a private/
// link-local/metadata (non-proxy) address, or a sensor.blind coverage gap. It
// never uses "denied": this is a DETECTION stream, it does not block. See
// internal/groundtruth for the canonical action/data definitions.
type AuditEvent struct {
	ID        uuid.UUID       `json:"id"`
	Time      time.Time       `json:"time"`
	RunID     *uuid.UUID      `json:"run_id,omitempty"`
	ActorType ActorType       `json:"actor_type"`
	Actor     string          `json:"actor"`  // human sub, agent SPIFFE ID, or component name
	Action    string          `json:"action"` // dotted verb, e.g. "credential.mint", "egress.deny", "kernel.process.exec"
	Target    string          `json:"target,omitempty"`
	Outcome   string          `json:"outcome"` // "success" | "failure" | "denied"
	SourceIP  string          `json:"source_ip,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`

	// PrevHash/RowHash are the tamper-evidence chain (migration 0047):
	// RowHash = SHA-256(PrevHash || canonical serialization of the fields
	// above), hex, computed BY POSTGRES in the audit_events BEFORE INSERT
	// trigger — never by the caller, who therefore cannot choose them.
	//
	// They are populated on the WRITE path only (InsertAuditEvent fills them
	// from RETURNING), which is what carries the current head hash out to the
	// audit sinks so a SIEM can detect a later truncation. The paginated READ
	// paths deliberately do not select them, so both are empty on anything
	// served by GET /audit — hence omitempty. The chain is verified through
	// GET /api/v1/audit/chain/verify, not by reading rows back.
	PrevHash string `json:"prev_hash,omitempty"`
	RowHash  string `json:"row_hash,omitempty"`
}

// SSHPublicKey is a human's registered public key for the SSH gateway
// (`GET`/`POST`/`DELETE /api/v1/me/ssh-keys`), distinct from GrantSpec's
// "ssh_key" grant kind (a RESIDENT private key materialized for git-over-SSH
// cloning — see GrantSSHKey). This type is the gateway's own trust root: a
// human self-registers a public key against their principal, and the gateway
// authenticates an incoming SSH connection ONLY against rows here, then
// authorizes it owner-OR-admin (AgentRun.CreatedBy == Principal, or Role ==
// oidc.RoleAdmin — see internal/api/sshgateway.go).
//
// Fingerprint is the SHA256 form (ssh.FingerprintSHA256: "SHA256:<base64>"),
// computed SERVER-SIDE from the parsed key — never client-supplied — so it is
// both the natural primary key (a key's fingerprint is intrinsic to its bytes;
// two rows can never disagree about which key they name) and the value shown
// to a human for "verify on first connect".
//
// Role is the registering session's OWN role (oidc.RoleAdmin/RoleMember),
// stamped at registration by handleAddSSHKey (migration 0043) and REFRESHED
// on every OIDC login for the authenticating principal's keys (migration
// 0046). It is a BOUNDED-STALE stamp, not a live check — SSH carries no
// session for requireOperator to read — so a demoted admin's key keeps its
// override only until RoleCheckedAt exceeds WARDYN_SSH_ROLE_TTL, or until the
// key is deleted/re-registered (docs/SSH.md §Bounds).
//
// RoleCheckedAt is when Role was last stamped — at registration, or at any
// later OIDC login (migration 0046). nil means "never refreshed since
// upgrading to 0046" — a pre-migration row, or a key registered before an
// OIDC-configured deployment's first login for that principal — and sshAuth
// treats nil as infinitely stale, never as fresh.
type SSHPublicKey struct {
	Fingerprint   string     `json:"fingerprint"`
	Principal     string     `json:"principal"`
	Name          string     `json:"name"`
	PublicKey     string     `json:"public_key"` // authorized_keys line; never a secret
	Role          string     `json:"role"`
	RoleCheckedAt *time.Time `json:"role_checked_at,omitempty"`
	CreatedAt     time.Time  `json:"created_at"`
}

// APIToken is one per-user API token (migration 0045): a long-lived bearer
// credential a HUMAN mints for their own scripts/CI so automation stops sharing
// the single deployment-wide admin token. The auth branch that accepts one
// (apiTokenAuth in internal/api/apitokens.go) republishes exactly the context
// a verified SSO session publishes, so grants, RBAC and run ownership bind to
// the OWNING HUMAN — never to the admin identity.
//
// Email/Role/Groups are a SNAPSHOT of the creating session, stamped at create
// time the way SSHPublicKey.Role is stamped at registration: a bearer token
// carries no ID token, so there is nothing to re-derive them from per request.
// The ceiling is the same one SSHPublicKey.Role carries — a demotion does not
// reach an outstanding token; REVOKE it.
//
// Groups distinguishes nil from empty exactly as the session path does (see
// oidcGroupsCtxKey in internal/api/http.go): nil means "snapshot unavailable",
// empty means "the IdP sent no usable groups".
//
// Token carries the PLAINTEXT credential and is populated on exactly one
// response — the create call — and is never stored, listed or logged. Every
// other path leaves it empty, and `omitempty` keeps it out of those bodies.
type APIToken struct {
	ID         uuid.UUID  `json:"id"`
	Principal  string     `json:"principal"`
	Email      string     `json:"email,omitempty"`
	Role       string     `json:"role"`
	Groups     []string   `json:"groups,omitempty"`
	Name       string     `json:"name"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
	Token      string     `json:"token,omitempty"` // plaintext, create response ONLY
}

// CapabilitySubjectType names WHO a capability grant is written against
// (migration 0042). Closed and complete — its DB CHECK is pinned against these
// constants by internal/db's TestClosedEnumChecksMatchConstants.
type CapabilitySubjectType string

const (
	// CapabilitySubjectUser is one human, named by either their lowercased OIDC
	// "sub" or their email — a grant on EITHER matches, so an admin can write
	// down the identity they actually know rather than the one the IdP prefers.
	CapabilitySubjectUser CapabilitySubjectType = "user"
	// CapabilitySubjectGroup is one entry of the login-time union of the ID
	// token's roles+groups claims (see oidc.Session.Groups). Entra App Roles are
	// grantable through this without any extra configuration.
	CapabilitySubjectGroup CapabilitySubjectType = "group"
	// CapabilitySubjectAll is every signed-in human — the baseline for an IdP
	// that emits no usable groups claim. Subject is "" for this type.
	CapabilitySubjectAll CapabilitySubjectType = "all"
)

// Valid reports whether t is one of the three subject types. Used to reject a
// garbage value at the API write boundary, mirroring ApprovalScope.Valid.
func (t CapabilitySubjectType) Valid() bool {
	switch t {
	case CapabilitySubjectUser, CapabilitySubjectGroup, CapabilitySubjectAll:
		return true
	default:
		return false
	}
}

// CapabilityEffect is a grant's direction. Closed and complete; DENY BEATS
// ALLOW at resolution time, with no user-vs-group precedence — "Bob's user
// allow overrode the group deny" is a breach report, not a feature.
type CapabilityEffect string

const (
	CapabilityAllow CapabilityEffect = "allow"
	CapabilityDeny  CapabilityEffect = "deny"
)

// Valid reports whether e is allow or deny.
func (e CapabilityEffect) Valid() bool {
	return e == CapabilityAllow || e == CapabilityDeny
}

// CapabilityGrant is one row of the permissioning grant list (migration 0042):
// "subject S may (or may not) use capability C at value V".
//
// Capability is a PLAIN STRING here, not a typed enum, and that is deliberate:
// the closed kind set lives in exactly one Go slice in internal/api
// (capabilityKinds) and is validated at the write boundary, so a fifth kind is
// a constant plus a call site with no schema change. A stored kind this binary
// does not know is inert — no resolver ever asks for it.
//
// Value's meaning is per kind: a host or "*.suffix" for egress_host, an exact
// secret name / workspace uuid / image ref for the others, and "*" is the
// per-kind wildcard everywhere.
type CapabilityGrant struct {
	ID          uuid.UUID             `json:"id"`
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	Capability  string                `json:"capability"`
	Value       string                `json:"value"`
	Effect      CapabilityEffect      `json:"effect"`
	CreatedAt   time.Time             `json:"created_at"`
	CreatedBy   string                `json:"created_by,omitempty"`
}
