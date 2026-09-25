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

// NonTerminalRunStates is IsTerminal's complement as a LIST, for the one caller
// that needs the set as data rather than as a predicate: the concurrent-run
// quota's SQL (store.CountActiveRunsBy).
//
// POSITIVE, never `NOT IN (terminal)`, and that direction is the safety. A
// state added to the enum and forgotten here undercounts by one state — the
// quota is merely looser than intended. Written as a negation, the same
// oversight would count a newly-added TERMINAL state as active and wedge every
// capped member at their limit forever, with no run they could stop to clear it.
//
// TestNonTerminalRunStatesPartitionTheEnum pins this against IsTerminal over
// every constant scanned from this file, so the two cannot disagree.
var NonTerminalRunStates = []RunState{RunPending, RunStarting, RunRunning, RunWaiting}

// LostReason says why a kept run lost its sandbox (AgentRun.LostReason).
type LostReason string

// LostEnded is a run whose lease ran out (AgentRun.EndsAt passed): stopped
// and kept for the ended-run grace.
const LostEnded LostReason = "ended"

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
	// StatusDetail is what the SUBSTRATE says this run is waiting on while it is
	// STARTING, in the substrate's own words — the
	// `<component>: <Reason>[: <message>]` shape the kubelet's container status,
	// the scheduler's PodScheduled condition and docker's first pull all render
	// into ("agent: ImagePullBackOff: …", "pod: Unschedulable: …", "image:
	// Pulling: <ref>"). Written from inside CreateSandbox (migration 0063,
	// runner.SandboxSpec.OnWaiting), once per CHANGE of reason, because the whole
	// STARTING window happens before the run has a sandbox_ref and nothing
	// outside the driver can ask a substrate anything.
	//
	// DISPLAY-ONLY, and never interpreted by the control plane: it is whatever
	// the platform said. It is also never CLEARED by a write — the read path
	// blanks it for any run that is not STARTING (except a run that FAILED on a
	// TERMINAL reason, where the reason IS the failure and a reader that missed
	// the last STARTING poll would otherwise see a reason-less FAILED badge), so
	// the last reason survives on the row for a postmortem without the console
	// ever narrating a finished run's old wait.
	StatusDetail string `json:"status_detail,omitempty"`
	// StatusReason is the bare reason token out of StatusDetail
	// (`ContainerCreating`, `ImagePullBackOff`, `Unschedulable`, `Pulling`,
	// `Pending`…) — DERIVED at read and never stored, the same way HasRecording
	// and its two siblings below are. It exists so that everything which is not a
	// person (metrics, the live specs, later automation) reasons on a token while
	// the console owns the sentence; the raw StatusDetail stays on the wire
	// because only it carries the registry's or the scheduler's own message.
	StatusReason string `json:"status_reason,omitempty"`
	// AutonomyLevel freezes the AutonomyLevel resolveRunAutonomy (0.8 #97)
	// resolved this run to at create time — the level, not the profile or the
	// posture that produced it (AutonomyResolution carries those, on the
	// create audit row only). Empty for a run under no profile, a profile with
	// no rubric, or any run created before this field existed; this is types,
	// validation, storage and mirrors only (#99) — nothing writes it yet, so
	// every run is empty until #97 lands. Migration 0065 adds the column NOT
	// NULL DEFAULT ''.
	AutonomyLevel AutonomyLevel `json:"autonomy_level,omitempty"`
	// EndsAt is when this run's lease ends (long-holds §2.1); nil is no end.
	// Captured at create from the owner's profile run limits (RunLimits) and
	// never re-resolved: the owner's authority at launch, not a later caller's.
	EndsAt *time.Time `json:"ends_at"`
	// WaitBudgetSec is how long a request this run raises stays open for a
	// decision. Captured at create, already folded under the deployment's
	// approval expiry; 0 (every run created before migration 0072) means the
	// deployment's approval expiry alone.
	WaitBudgetSec int `json:"wait_budget_sec,omitempty"`
	// RunLimits are the owner's profile run limits as they stood at create, and
	// GovernanceProfileID the profile they came from (nil for an unassigned or
	// super-admin owner). Every later change to the end or the wait clamps
	// against these captured bounds, never against the profile's current ones.
	RunLimits           RunLimits  `json:"run_limits"`
	GovernanceProfileID *uuid.UUID `json:"governance_profile_id,omitempty"`
	// LostAt / LostReason mark a run that lost its sandbox but is KEPT: its
	// agent is stopped and its proxy gone, so it has no network, while its
	// files stay. The run keeps its RunState (RUNNING, so it still holds a quota
	// slot); only a kill or the ended-run grace makes it terminal. Nil / "" is a
	// live run. Migration 0073.
	LostAt     *time.Time `json:"lost_at,omitempty"`
	LostReason LostReason `json:"lost_reason,omitempty"`
	// HasRecording, RecordingBytes and RecordingDurationSec are
	// DERIVED, never stored: projected by handleListRuns/handleGetRun from
	// RecordingStore.StatAndTail(id) after the store read — but ONLY when the
	// request opts in with ?include=recording_meta (wantsRecordingMeta,
	// runs_recording_meta.go): it costs one StatAndTail call per run in the
	// response, and handleListRuns also backs the Runs board's 3s poll at
	// limit=1000, which renders none of these three fields. Without the flag
	// (or with RecordingStore nil) all three sit at their zero value, same as
	// "not requested". RecordingBytes is the exact payload size;
	// RecordingDurationSec is the last captured output frame's elapsed time,
	// read from a small tail of the payload (see
	// internal/recording.LastOutputElapsed) — the same number recordings.ts's
	// former client-side probe computed by fetching the WHOLE document.
	// HasRecording is the ONLY "no recording" signal: a zero
	// RecordingDurationSec on a has_recording=true run is a real, header-only
	// cast that captured no output, never "unknown" or "none".
	HasRecording         bool    `json:"has_recording"`
	RecordingBytes       int64   `json:"recording_bytes,omitempty"`
	RecordingDurationSec float64 `json:"recording_duration_sec,omitempty"`
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
	// WARDYN_ALLOW_USER_ENV_SECRET. Scope is {"name":"MY_TOKEN",
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

// AWSSSOAccessTokenSecret is a SENTINEL secret name (NOT a stored secret) that
// works exactly like the two OAuth sentinels above at the injection sink
// (host-pinned, forced header, masked), and resolves to the run's OWN captured
// AWS IAM Identity Center access token -- the credential an `aws sso login`
// captured into wardynd's secret store as a whole blob, not as a value under
// this name.
//
// It is what makes Phase B (0.7.6) possible: portal.sso.<region>
// GetRoleCredentials is `authtype:none`, so the proxy can carry the session as
// the x-amz-sso_bearer_token HEADER and the sandbox never holds the token. The
// resolver is internal/api's resolveAWSSSOInjection, which additionally
// re-derives the credential's scope from the roster and refuses any drift from
// the snapshot the grant was authored with.
//
// Deliberately NOT in sinkReservedSecret. That guard refuses names an api_key
// grant must never resolve; this name's ENTIRE purpose is to be resolved
// through that sink, host-pinned, exactly as bedrock-api-key is (see
// internal/api/secrets.go, which explains why that one is excluded too). And
// nothing is stored under it: a secrets-API Put of this name would be a value
// the sentinel arm never reads.
const AWSSSOAccessTokenSecret = "aws-sso-access-token"

// ADOEntraAccessTokenSecret is a SENTINEL secret name (NOT a stored secret),
// the fourth of them, and it resolves to an Azure DevOps access token minted
// from the RUN OWNER's own captured Entra sign-in (internal/api's
// resolveADOInjection). Nothing is stored under this name: the credential in
// the store is the person's refresh token, held in the reserved
// `wardyn-harness-ado-<row>-oauth` blob, and the access token exists only for
// the moment it is handed to the run's proxy sidecar.
//
// THE TOKEN DOES NOT BOUND THE RUN, and no reader of this name may assume it
// does. Measured against a real tenant, Entra issues an Azure DevOps token
// carrying EVERY scope the person consented to whatever subset is requested,
// so what holds a run to its granted capabilities is Wardyn's own capability
// check in front of the resource — the proxy — not the credential. The
// granted scope string the authority reports is recorded on the audit row
// because a token is opaque and that string is the only honest evidence of
// what the credential can do.
//
// Deliberately NOT in sinkReservedSecret, for AWSSSOAccessTokenSecret's
// reason: being resolved at that sink, host-pinned, is the whole point.
const ADOEntraAccessTokenSecret = "azure-devops-entra-access-token"

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
	// Organisation is the Azure DevOps organisation a per-person Azure DevOps
	// credential was dispatched for, on that lane's resolves only (empty on
	// every other). It is informational: the proxy does not read it. The
	// proxy's REST gate pins the organisation from the dispatch-time ADOGrant
	// in its own configuration (proxy.ADOGrantConfig), never from a resolve.
	Organisation string `json:"organisation,omitempty"`
	// Capabilities is the same lane's GRANTED capability set, in the
	// internal/adoscope vocabulary. The gate does NOT hold requests to it: it
	// reads the dispatch-time ADOGrant. The proxy reads it in one place only,
	// on a capability ask's resolve (ado_hold.go), to confirm the capability it
	// asked for came back granted. Empty on every other lane.
	Capabilities []string `json:"capabilities,omitempty"`
}

// ApprovalKind enumerates what a human is being asked to approve.
type ApprovalKind string

const (
	ApprovalCredential   ApprovalKind = "credential"
	ApprovalEgressDomain ApprovalKind = "egress_domain"
	ApprovalToolCall     ApprovalKind = "tool_call"
	// ApprovalCredentialReauth: the run's own model credential lapsed MID-RUN
	// and cannot be renewed, so the proxy is HOLDING the sandbox's next
	// credential exchange while the credential's owner signs in again
	// (internal/api/injection_awssso.go, internal/egress/proxy/credhold.go).
	//
	// It is NOT a decision. Nobody approves or denies it: it is resolved by the
	// owner completing the sign-in, which is why Server.decide answers 409 for
	// this kind and the console renders a door instead of an Approve/Deny pair.
	// The row still moves to APPROVED — so every existing list, count and
	// terminal-cascade reader works unchanged — but through ResolveReauth and
	// its own credential.reauth.resolve audit action, never approval.decide.
	ApprovalCredentialReauth ApprovalKind = "credential_reauth"
	// ApprovalPushContent: a brokered git push touched a path the run's
	// push_rules.require_review_paths names, and the proxy is HOLDING it while
	// an admin decides (internal/egress/proxy/push_hold.go). Its requested
	// scope is a PushContentScope. Admin-decidable only: a member deciding
	// their own run's workflow-file edit is the exfiltration the rule stops,
	// so authorizeMemberDecision keeps members to egress_domain.
	ApprovalPushContent ApprovalKind = "push_content"
)

// ApprovalKinds is the closed set. It exists for the same reason
// ApprovalStates does, and it was missing for longer: internal/db's
// closedEnumChecks covered approvals.state and approvals.decision_scope but NOT
// approvals.kind, so a Go constant the database CHECK rejects would have left
// every gate green. TestApprovalKindsCoversEveryConstant keeps it honest
// against the constants above.
var ApprovalKinds = []ApprovalKind{
	ApprovalCredential, ApprovalEgressDomain, ApprovalToolCall, ApprovalCredentialReauth,
	ApprovalPushContent,
}

// ApprovalState is the approval lifecycle.
type ApprovalState string

const (
	ApprovalPending  ApprovalState = "PENDING"
	ApprovalApproved ApprovalState = "APPROVED"
	ApprovalDenied   ApprovalState = "DENIED"
	ApprovalExpired  ApprovalState = "EXPIRED"
	// ApprovalCancelled: the RUN this approval belongs to reached a terminal
	// state (killed, completed, failed, stopped) while the approval was still
	// PENDING, so the question it asked can no longer be answered — approving it
	// would mint a credential for a sandbox that is gone. Distinct from DENIED
	// (a human refused) and from EXPIRED (a sweeper aged it out undecided):
	// nobody decided this one and nothing was refused. Terminal, like the other
	// three, and every reader treats it as a refusal to proceed.
	ApprovalCancelled ApprovalState = "CANCELLED"
)

// ApprovalStates is the closed set, in lifecycle order (the one non-terminal
// state first, then the four terminal ones). It exists so the migration parity
// guard (internal/db's closedEnumChecks) compares the database's CHECK against
// the GO SET rather than against a hand-typed list in the test — the blindness
// the role_mappings.role case argues must be closed by derivation, and the one
// CHANGELOG 0062 advertises as closed. TestApprovalStatesCoversEveryConstant
// keeps this slice honest against the constants above.
var ApprovalStates = []ApprovalState{
	ApprovalPending, ApprovalApproved, ApprovalDenied, ApprovalExpired, ApprovalCancelled,
}

// The approval sentinels live here, in the one package both internal/store and
// internal/approval already import, so the FSM can errors.Is a store error
// instead of matching its message text (matching text silently breaks
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
	// ExpiresAt is when a PENDING request stops waiting:
	// min(requested_at + the run's WaitBudgetSec, the run's EndsAt). Computed
	// on read from the run row, never stored or accepted from a caller, so a
	// change to the run's end or wait reaches its open requests at once. Nil
	// when the run has neither (a run created before migration 0072); the
	// deployment's approval expiry still applies to every row.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
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
//	                  kernel.sensor.ping     — sensor liveness (run_id NULL)
//	                  kernel.sensor.bypass   — host eBPF blind to a run (CC3/Kata)
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
//	  "dropped_total": <uint64>,        // heartbeat only: events lost before
//	                                    // reaching the control plane — POST
//	                                    // backpressure OR an oversized/
//	                                    // unterminated export line (B12b-F9)
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

	// DeviceID names the enrolled device that forwarded this row, and is nil
	// on every row the organisation wrote itself. Derived on read from the
	// stored row (store.FederatedDeviceID), never a column and never taken
	// from a caller: a device that claims one is refused.
	DeviceID *uuid.UUID `json:"device_id,omitempty"`

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
// Role is the registering session's OWN role (oidc.RoleAdmin/RoleUser),
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
//
// Capped marks a key registered while an admin's session was in the user view
// (migration 0070): Role stays user through every login re-stamp, and
// sshAuth never grants it the admin override.
type SSHPublicKey struct {
	Fingerprint   string     `json:"fingerprint"`
	Principal     string     `json:"principal"`
	Name          string     `json:"name"`
	PublicKey     string     `json:"public_key"` // authorized_keys line; never a secret
	Role          string     `json:"role"`
	RoleCheckedAt *time.Time `json:"role_checked_at,omitempty"`
	Capped        bool       `json:"capped"`
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
// GroupsTruncated is that snapshot's PF-26 completeness marker (migration
// 0052's api_tokens.groups_truncated), and it is THREE-VALUED on purpose:
//
//	true  — the snapshot was cut off by the cookie byte cap at mint.
//	false — the snapshot is complete.
//	nil   — UNKNOWN: a token minted before 0.7, when nothing recorded the bit.
//
// nil is NOT false. A pre-0.7 token's snapshot may well have been truncated,
// and reading it as complete lets the holder silently shed a group-assigned
// governance profile — the exact evaporation this marker closes. Consumers
// treat nil as TRUNCATED (fail closed); the cost is one re-mint, and only on a
// deployment that actually assigns profiles to groups.
type APIToken struct {
	ID              uuid.UUID  `json:"id"`
	Principal       string     `json:"principal"`
	Email           string     `json:"email,omitempty"`
	Role            string     `json:"role"`
	Groups          []string   `json:"groups,omitempty"`
	GroupsTruncated *bool      `json:"groups_truncated,omitempty"`
	Name            string     `json:"name"`
	CreatedAt       time.Time  `json:"created_at"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	Token           string     `json:"token,omitempty"` // plaintext, create response ONLY
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

// RoleMapping is one console-managed (Getting Started -> People) row of
// migration 0051's role_mappings table: "value maps to role". This is the
// STORE'S wire type, carrying id/timestamps/provenance — distinct on purpose
// from internal/auth/oidc's own RoleMapping (just Value/Role), which stays
// dependency-free of this package (see that type's doc comment) the same way
// oidc.SessionRevocations keeps oidc dependency-free of store; the API layer
// converts between the two, mirroring however SessionRevocations bridges
// store -> oidc today.
//
// Value is expected already canonical (trimmed, lowercased, ASCII) by the API
// write boundary that owns writes to this table — see the migration comment.
//
// UserType is the row's user type when Role is the user tier (migration
// 0070_user_tier_rename's column, a foreign key to user_types); "" on a tier
// row, and on a user row written before types existed, which reads as the
// built-in "standard".
type RoleMapping struct {
	ID        uuid.UUID `json:"id"`
	Value     string    `json:"value"`
	Role      string    `json:"role"`
	UserType  string    `json:"user_type,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}
