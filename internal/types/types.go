// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package types defines Wardyn's core domain vocabulary — AgentRun, RunPolicy, CredentialGrant,
// ApprovalRequest, and the audit event shape — shared by the control plane, runners, sidecars, and the CRD layer.
package types

import (
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
)

// ConfinementClass declares how strongly a sandbox substrate confines an agent; policy may refuse
// high-trust credential scopes to weak classes. See threatmodel/.
type ConfinementClass string

const (
	// CC1: hardened shared-kernel runc (userns, seccomp, AppArmor, cap-drop).
	CC1 ConfinementClass = "CC1"
	// CC2: gVisor userspace kernel (the default — needs a native Docker engine with runsc registered).
	CC2 ConfinementClass = "CC2"
	// CC3: Kata microVM (requires /dev/kvm).
	CC3 ConfinementClass = "CC3"
)

// Rank orders classes weakest→strongest; unrecognised values rank 0 so a minimum-class gate fails
// closed. Matching is exact — normalise untrusted input first.
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

// ConfinementClassNames maps each wire code to its UI display label (mirrors cc-meta.ts CC_META).
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
	// RunWaiting is reserved: unused, but kept wired end-to-end for the approval-gated-run feature.
	RunWaiting  RunState = "WAITING_FOR_CONFIRMATION"
	RunStopped  RunState = "STOPPED"
	RunArchived RunState = "ARCHIVED"
	RunFailed   RunState = "FAILED"
	RunKilled   RunState = "KILLED"
	// RunCompleted is terminal success (exit 0); a non-zero exit becomes RunFailed instead.
	RunCompleted RunState = "COMPLETED"
)

// IsTerminal reports whether the run has ended. SINGLE source of the terminal set — the UI's copy
// (runs.ts) is pinned equal by TestTerminalRunStates_UIParity.
func (s RunState) IsTerminal() bool {
	switch s {
	case RunCompleted, RunFailed, RunKilled, RunStopped, RunArchived:
		return true
	}
	return false
}

// NonTerminalRunStates is IsTerminal's complement, for the active-run quota query. POSITIVE, never
// `NOT IN (terminal)`: a forgotten member only undercounts here, but a negation would wedge capped
// accounts at their limit forever. Pinned against IsTerminal by test.
var NonTerminalRunStates = []RunState{RunPending, RunStarting, RunRunning, RunWaiting}

// PauseReason says why a run's agent is frozen (AgentRun.PausedReason).
type PauseReason string

const (
	PauseWaiting PauseReason = "waiting" // parked on an open request nobody has answered
	PauseIdle    PauseReason = "idle"    // unused past pause_idle_after_sec, CPU quiet
)

// ActorType distinguishes who performed an action in the audit stream.
type ActorType string

const (
	ActorHuman  ActorType = "human"
	ActorAgent  ActorType = "agent"
	ActorSystem ActorType = "system"
)

// AgentRun is one governed execution of a coding agent on behalf of a human, with its own identity
// (SPIFFE ID), credential grants, and audit trail.
type AgentRun struct {
	ID        uuid.UUID `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by"` // human principal (token `sub`)
	Agent     string    `json:"agent"`      // e.g. "claude-code", "codex-cli"
	Repo      string    `json:"repo"`       // e.g. "org/name"
	Task      string    `json:"task"`       // human task description
	// Title groups runs in the console's list; empty for legacy/system runs, which fall back to Task.
	Title string `json:"title,omitempty"`
	// Description is optional free-text context; display-only, never interpreted.
	Description      string           `json:"description,omitempty"`
	PolicyID         *uuid.UUID       `json:"policy_id,omitempty"`
	ConfinementClass ConfinementClass `json:"confinement_class"`
	State            RunState         `json:"state"`
	SPIFFEID         string           `json:"spiffe_id"`     // spiffe://<trust-domain>/agent-run/<id>
	RunnerTarget     string           `json:"runner_target"` // "docker"
	SandboxRef       string           `json:"sandbox_ref,omitempty"`
	Image            string           `json:"image,omitempty"` // resolved sandbox image, kept for provenance; empty for legacy rows
	// Interactive marks a human-driven run: sandbox comes up RUNNING, no task exec'd, no completion watcher.
	Interactive bool `json:"interactive"`
	// WorkspacePath is the run's primary host directory, denormalized so the control plane can warn (never block) a second run on it.
	WorkspacePath string `json:"workspace_path,omitempty"`
	// WorkspaceID marks a governed scan run: the driver runs wardyn-scan instead of the agent, trusted (not sandbox input) to persist the profile.
	WorkspaceID *uuid.UUID `json:"workspace_id,omitempty"`
	// WorkspaceIDs is the onboarded workspaces this run's policy referenced at create; grants nothing, only tells an `always` egress decision what to persist to.
	WorkspaceIDs []uuid.UUID `json:"workspace_ids,omitempty"`
	SourceID     *uuid.UUID  `json:"source_id,omitempty"` // trusted run→library-source linkage for a per-source scan run; never set by user runs
	// AutoStopAfterSec is the run's idle auto-stop cap, frozen at create so the reaper works with no policy_id to JOIN. 0 = never; <0 = explicit never.
	AutoStopAfterSec int    `json:"auto_stop_after_sec,omitempty"`
	AgentExecID      string `json:"agent_exec_id,omitempty"` // docker exec id, so the crash reconciler can check agent liveness across a wardynd restart
	FailureHint      string `json:"failure_hint,omitempty"`  // operator-facing reason a run FAILED before the agent started; display-only
	// StatusDetail is what the substrate says this run awaits while STARTING; blanked on leaving STARTING except a TERMINAL failure reason (kept for postmortem).
	StatusDetail string `json:"status_detail,omitempty"`
	StatusReason string `json:"status_reason,omitempty"` // reason token parsed out of StatusDetail, derived at read, never stored
	// AutonomyLevel freezes the level resolved at create, not the profile/posture that produced it (that's on the audit row).
	AutonomyLevel AutonomyLevel `json:"autonomy_level,omitempty"`
	EndsAt        *time.Time    `json:"ends_at"`                   // run's lease end, captured once at create from RunLimits, never re-resolved
	WaitBudgetSec int           `json:"wait_budget_sec,omitempty"` // how long an approval this run raises stays open; 0 = deployment default alone applies
	// RunLimits are the owner's profile limits as they stood at create; later clamps use these captured bounds, never the profile's current ones.
	RunLimits           RunLimits  `json:"run_limits"`
	GovernanceProfileID *uuid.UUID `json:"governance_profile_id,omitempty"`
	// LostAt/LostReason mark a run that lost its sandbox but is kept (no proxy/network, files stay); stays RUNNING until a kill or the ended-run grace.
	LostAt     *time.Time `json:"lost_at,omitempty"`
	LostReason LostReason `json:"lost_reason,omitempty"`
	// ContainmentError/At: latest/first stop-confirm failure for a kept run; the lease sweep retries and clears both once it lands.
	ContainmentError   string     `json:"containment_error,omitempty"`
	ContainmentErrorAt *time.Time `json:"containment_error_at,omitempty"`
	// EndedAt stamps the transition into a terminal RunState; a lease-ended run stays RUNNING until the ended-run grace, so its end time is LostAt, not EndedAt.
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	EndTightenedAt *time.Time `json:"end_tightened_at,omitempty"` // profile re-clamp shortened the end
	// PausedAt/PausedReason mark a run frozen with nobody present; keeps state/memory/files/proxy until activity or resume. Not contained — kill still works.
	PausedAt     *time.Time  `json:"paused_at,omitempty"`
	PausedReason PauseReason `json:"paused_reason,omitempty"`
	ActiveAt     *time.Time  `json:"active_at,omitempty"` // presence clock: last human input/egress decision/proxied bytes; keepalives never move it; nil reads as CreatedAt
	// ModelProviderID freezes the provider id at create; the kind is deliberately NOT frozen (can change later) and lives only on the run.create audit snapshot.
	ModelProviderID string `json:"model_provider_id,omitempty"`
	UserType        string `json:"user_type,omitempty"` // creator's resolved user type, frozen at create
	Preset          string `json:"preset,omitempty"`    // launch preset this run was expanded from; empty for an explicit-spec run
	PresetVersion   int    `json:"preset_version,omitempty"`
	OperatorOwned   bool   `json:"operator_owned,omitempty"` // mirrors identity.Claims.OperatorOwned so dispatch/revive re-mint read what create decided; the console reads it for the no-personal-owner entry carve-out
	// CreatedVia is the registered portal this run was launched through on the person's behalf; nil when self-launched. The owner is still the person.
	CreatedVia *uuid.UUID `json:"created_via,omitempty"`
	// HasRecording/RecordingBytes/RecordingDurationSec are derived, never stored, projected only with ?include=recording_meta; HasRecording is the only "no recording" signal.
	HasRecording         bool          `json:"has_recording"`
	RecordingBytes       int64         `json:"recording_bytes,omitempty"`
	RecordingDurationSec float64       `json:"recording_duration_sec,omitempty"`
	DiskMiB              int           `json:"disk_mib,omitempty"`  // effective ephemeral disk cap MiB; 0 = no cap resolved
	Attention            *RunAttention `json:"attention,omitempty"` // what this live run awaits and who can clear it; derived, only on view-opted-in read paths
}

// GrantKind enumerates broker-mintable credential kinds.
type GrantKind string

const (
	GrantGitHubToken GrantKind = "github_token"
	GrantCloudSTS    GrantKind = "cloud_sts" // HARD-REQUIRES SPIRE identity provider
	GrantAPIKey      GrantKind = "api_key"   // proxy-side injection only
	// GrantGitPAT returns a stored PAT to the git credential helper for a non-GitHub host (ADO/GitLab), an opaque CONNECT tunnel the proxy can't inject into.
	GrantGitPAT GrantKind = "git_pat"
	// GrantSSHKey materializes a resident, agent-readable SSH private key for git-over-SSH — a DOCUMENTED EXCEPTION to the no-resident-secret invariant
	// (no credential-helper seam for SSH). agent-run writes it 0400 just before the clone and wipes it right after; residual risk is code running as the
	// agent uid during that window. See broker.mintSSHKey.
	GrantSSHKey GrantKind = "ssh_key"
	// GrantEnvSecret places a stored secret into the sandbox environment at dispatch — the second DOCUMENTED EXCEPTION to the no-resident-secret invariant.
	// Unlike every other kind: NOT BROKERED (no mint/approval/TTL/JTI); RESIDENT FOR THE WHOLE RUN (readable via /proc/self/environ as the agent uid);
	// NO REVOCATION (killing the run doesn't un-disclose it). ADMIN-ONLY by default — see threatmodel/THREAT-MODEL.md §5.1a.
	GrantEnvSecret GrantKind = "env_secret"
)

// GrantSpec is a credential scope description. The broker enforces the invariant: a minted
// credential's scope is exactly the approved scope — never wider.
type GrantSpec struct {
	Kind GrantKind `json:"kind"`
	// Scope is kind-specific: {repos,permissions} for github_token; {host,header} for api_key;
	// {host,secret_name,username?} for git_pat; {host,key_secret_ref,username?,known_hosts_secret_ref?} for ssh_key.
	Scope json.RawMessage `json:"scope"`
	// TTL of the minted credential. Max (and default) 1h.
	TTLSeconds int `json:"ttl_seconds,omitempty"`
	// RequiresApproval forces a human approval to mint (vs auto-mint on policy).
	RequiresApproval bool `json:"requires_approval"`
	OwnerOnly        bool `json:"owner_only,omitempty"` // stored secret from the run owner's own row only, never the operator's
}

// CredentialGrant records what a run is ELIGIBLE for — not issuance: minting happens only via the
// broker, and for RequiresApproval grants only inside the transaction that verifies an APPROVED
// ApprovalRequest for this run+scope.
type CredentialGrant struct {
	ID        uuid.UUID `json:"id"`
	RunID     uuid.UUID `json:"run_id"`
	CreatedAt time.Time `json:"created_at"`
	Spec      GrantSpec `json:"spec"`
}

// ResolvedInjection is the wire contract of GET /api/v1/internal/injection/{grantID}: header name
// plus the server-formatted secret value. ExpiresAt (unix ms, 0 = never) marks a credential the
// proxy must re-resolve before it lapses; only a single-use approval-gated grant leaves it 0.
type ResolvedInjection struct {
	Host      string `json:"host"`
	Header    string `json:"header"`
	Value     string `json:"value"`
	JTI       string `json:"jti"`
	ExpiresAt int64  `json:"expires_at,omitempty"`
	// Organisation is the ADO org a per-person credential was dispatched for — informational only;
	// the proxy's REST gate pins the org from its own dispatch-time config, never from a resolve.
	Organisation string `json:"organisation,omitempty"`
	// Capabilities is the same lane's granted capability set, read once to confirm an asked-for
	// capability came back granted. Empty on other lanes.
	Capabilities []string `json:"capabilities,omitempty"`
}

// ApprovalKind enumerates what a human is being asked to approve.
type ApprovalKind string

const (
	ApprovalCredential   ApprovalKind = "credential"
	ApprovalEgressDomain ApprovalKind = "egress_domain"
	ApprovalToolCall     ApprovalKind = "tool_call"
	// ApprovalCredentialReauth: the run's model credential lapsed mid-run, so the proxy holds the
	// next exchange while the owner signs in again. Not a decision — the row moves to APPROVED only
	// via ResolveReauth, never approval.decide.
	ApprovalCredentialReauth ApprovalKind = "credential_reauth"
	// ApprovalPushContent: a brokered git push touched a require_review_paths path, held for an
	// admin decision. Admin-only — a member deciding their own push is the exfiltration it stops.
	ApprovalPushContent ApprovalKind = "push_content"
)

// ApprovalKinds is the closed set, checked by test against the constants above — closedEnumChecks
// once missed approvals.kind entirely, which would have let a DB-rejected constant pass every gate green.
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
	// ApprovalCancelled: the run reached a terminal state while still PENDING; answering it would
	// mint a credential for a gone sandbox. Every reader treats it as a refusal.
	ApprovalCancelled ApprovalState = "CANCELLED"
)

// ApprovalStates is the closed set, in lifecycle order, so closedEnumChecks compares the DB CHECK
// against the Go set rather than a hand-typed test list.
var ApprovalStates = []ApprovalState{
	ApprovalPending, ApprovalApproved, ApprovalDenied, ApprovalExpired, ApprovalCancelled,
}

// These sentinels live here (shared by internal/store and internal/approval) so the FSM can
// errors.Is instead of matching message text, which silently breaks on rewording.
var (
	// ErrApprovalAlreadyDecided: a decision was attempted on an approval that already left PENDING.
	// Fail closed — never let a second decision overwrite the first.
	ErrApprovalAlreadyDecided = errors.New("approval already decided")
	// ErrDuplicatePendingApproval: a partial unique index rejected a second open PENDING approval for
	// the same dedup key — a dedup signal (re-read the winner), not a hard failure.
	ErrDuplicatePendingApproval = errors.New("duplicate pending approval")
)

// ApprovalScope is how far a human's approve/deny decision reaches — orthogonal to FirstUseMode,
// which decides whether to escalate at all. egress_domain carries any of the four; tool_call
// carries none; credential carries only ScopeRun, re-mintable for the rest of the run.
type ApprovalScope string

const (
	// ScopeOnce releases exactly one connection — one CONNECT tunnel for HTTPS, one request for HTTP.
	ScopeOnce ApprovalScope = "once"
	// ScopeRun holds for the rest of the run — the legacy default meaning.
	ScopeRun ApprovalScope = "run"
	// ScopeUntil holds until DecisionExpiresAt or run end, whichever comes first.
	ScopeUntil ApprovalScope = "until"
	// ScopeAlways also persists the host onto the run's workspace so future runs inherit it. Operator-only.
	ScopeAlways ApprovalScope = "always"
)

// Valid reports whether s is empty (legacy run-scoped) or a known scope — rejects garbage at the
// API write boundary; runtime reads still fail closed via Normalize.
func (s ApprovalScope) Valid() bool {
	switch s {
	case "", ScopeOnce, ScopeRun, ScopeUntil, ScopeAlways:
		return true
	default:
		return false
	}
}

// Normalize resolves a stored/wire value for every runtime read: empty means legacy run-scoped =>
// ScopeRun; an unknown non-empty value can only come from a newer control plane, and treating it as
// ScopeRun would be fail-open across versions, so it floors to ScopeOnce. Caveat: on a DENY, once is
// actually WIDER (it re-raises), so this isn't uniformly fail-closed, just re-denied with extra queue noise.
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

// ApprovalRequest is a blocking human-in-the-loop gate. RequestedScope is EXACTLY what the
// approver saw; the broker writes MintedJTI back in the same transaction as the mint.
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
	// DecisionScope is how far the decision reaches (see ApprovalScope); empty on a PENDING row. Named
	// decision_scope to avoid clashing with RequestedScope (the unrelated host JSON).
	DecisionScope ApprovalScope `json:"decision_scope,omitempty"`
	// DecisionExpiresAt is set only when DecisionScope is until — a different clock than the EXPIRED-state sweeper, which ages out stale PENDING requests.
	DecisionExpiresAt *time.Time `json:"decision_expires_at,omitempty"`
	// ExpiresAt is when a PENDING request stops waiting: min(requested_at+WaitBudgetSec, EndsAt); computed on read, never stored.
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Held      bool       `json:"held,omitempty"` // Held/HeldUntil project approval.Hold(this, now) at response time, never stored; both zero on a DECIDED row
	HeldUntil *time.Time `json:"held_until,omitempty"`
}

// ApprovalDecision is what a human (or the sweeper, for a stale-PENDING expiry) is deciding,
// threaded as one value instead of growing DecideApproval's signature. ExpireStale leaves Scope at
// "" (never ScopeRun) — an expiry is a sweep nobody decided.
type ApprovalDecision struct {
	State     ApprovalState
	DecidedBy string
	Reason    string
	Scope     ApprovalScope // meaningful only for an egress_domain approval; others leave the zero value
	ExpiresAt *time.Time
}

// AuditEvent is one append-only audit record. Every credential mint/revoke, approval decision,
// policy change, egress decision, and lifecycle change emits one, carrying the delegation chain
// (human sub + agent run). credential.*/identity.*/approval.*/policy.*/egress.*/run.*/recording.*
// are control-plane and agent self-report events; kernel.* is the eBPF/Tetragon ground-truth
// stream, emitted ONLY by the host-scoped sensor via POST /api/v1/internal/groundtruth, which
// FORCES actor_type=system+actor="wardyn-tetragon-ingest" and rejects any action without the
// "kernel." prefix. kernel.* Data carries stream/subtype/cgroup_id/container_id, a kind-specific
// argv/dst/path, loader (the documented mmap execve-hook bypass surface, flagged not blocked), and
// correlation ("mapped"|"unmapped" — unmapped => run_id NULL, never silently dropped). Outcome
// stays within "success"|"failure"|"denied": kernel events use failure for an escape/coverage-gap
// signal and never denied — a detection stream, it does not block. See internal/groundtruth.
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

	// DeviceID names the enrolled device that forwarded this row; derived on read, never taken from a caller.
	DeviceID *uuid.UUID `json:"device_id,omitempty"`

	// PrevHash/RowHash form the tamper-evidence chain: RowHash = SHA-256(PrevHash || canonical serialization
	// of the fields above), computed BY POSTGRES in a BEFORE INSERT trigger, never by the caller; sinks use
	// the head hash to detect truncation. Verified via GET /api/v1/audit/chain/verify.
	PrevHash string `json:"prev_hash,omitempty"`
	RowHash  string `json:"row_hash,omitempty"`
}

// SSHPublicKey is a human's registered public key for the SSH gateway, distinct from GrantSpec's
// ssh_key grant (a resident private key for git-over-SSH — see GrantSSHKey). This is the gateway's
// trust root: it authenticates an incoming connection ONLY against rows here, then authorizes
// owner-or-admin. Fingerprint is computed server-side from the parsed key, never client-supplied.
// Role is the registering session's own role, stamped at registration and refreshed on every OIDC
// login — a BOUNDED-STALE stamp, not a live check: a demoted admin's key keeps its override until
// RoleCheckedAt exceeds WARDYN_SSH_ROLE_TTL, or re-registration; nil RoleCheckedAt reads as
// infinitely stale. Capped marks a key registered while an admin's session was in the user view:
// Role stays user through every re-stamp, and the admin override is never granted.
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

// APIToken is a per-user, long-lived bearer credential a human mints for scripts/CI, replacing
// shared use of the deployment-wide admin token; grants/RBAC/ownership bind to the owning human.
// Email/Role/Groups are a snapshot of the creating session (a bearer token carries no ID token to
// re-derive them from) — same bounded-stale ceiling as SSHPublicKey.Role: a demotion doesn't reach
// an outstanding token; revoke it. Groups distinguishes nil ("snapshot unavailable") from empty
// ("IdP sent no usable groups"); UserType change on the People page revokes tokens rather than
// re-stamping them. Token carries the plaintext credential, populated only on the create response
// — never stored, listed or logged. GroupsTruncated is THREE-VALUED: true = cut off by the cookie
// byte cap at mint; false = complete; nil = unknown (pre-dates this bit) and reads as TRUNCATED
// (fail closed) — nil-as-false would let a holder silently shed a group-assigned governance profile.
type APIToken struct {
	ID              uuid.UUID  `json:"id"`
	Principal       string     `json:"principal"`
	Email           string     `json:"email,omitempty"`
	Role            string     `json:"role"`
	UserType        string     `json:"user_type"`
	Groups          []string   `json:"groups,omitempty"`
	GroupsTruncated *bool      `json:"groups_truncated,omitempty"`
	Name            string     `json:"name"`
	CreatedAt       time.Time  `json:"created_at"`
	LastUsedAt      *time.Time `json:"last_used_at,omitempty"`
	RevokedAt       *time.Time `json:"revoked_at,omitempty"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"` // nil = never expires; an expired token authenticates nothing
	MintedBy        string     `json:"minted_by,omitempty"`  // admin who minted this for its owner; empty when the owner minted it
	Token           string     `json:"token,omitempty"`      // plaintext, create response ONLY
	// IdentityStampedAt is when Role and Groups were last stamped: at mint, then at each sign-in of
	// the owner. Nil reads as stale under WARDYN_ROLE_STAMP_TTL. Never on the wire.
	IdentityStampedAt *time.Time `json:"-"`
}

// Person is an identity an admin created or confirmed before its first sign-in, keyed by the
// identity provider's subject — or, on Entra ID, by Issuer, TenantID and ObjectID, all three set
// or none (#1195).
type Person struct {
	Principal       string     `json:"principal"`
	Email           string     `json:"email,omitempty"`
	Issuer          string     `json:"issuer,omitempty"`
	TenantID        string     `json:"tenant_id,omitempty"`
	ObjectID        string     `json:"object_id,omitempty"`
	CreatedBy       string     `json:"created_by"`
	CreatedAt       time.Time  `json:"created_at"`
	FirstSignedInAt *time.Time `json:"first_signed_in_at,omitempty"`
}

// PersonSummary is one row of GET /people: a person this deployment knows, with the counts behind
// the leaver actions. ActiveSessions is 0 or 1: sessions are stateless cookies, so it says whether
// the last sign-in could still hold a live one, not how many cookies are out.
type PersonSummary struct {
	Principal       string     `json:"principal"`
	Email           string     `json:"email,omitempty"`
	IssuerKind      string     `json:"issuer_kind"`
	PreCreated      bool       `json:"pre_created"`
	FirstSignedInAt *time.Time `json:"first_signed_in_at,omitempty"`
	LastSignedInAt  *time.Time `json:"last_signed_in_at,omitempty"`
	DeactivatedAt   *time.Time `json:"deactivated_at,omitempty"`
	Role            string     `json:"role,omitempty"`
	ActiveSessions  int        `json:"active_sessions"`
	APITokens       int        `json:"api_tokens"`
	SSHKeys         int        `json:"ssh_keys"`
	Credentials     int        `json:"credentials"`
	ActiveRuns      int        `json:"active_runs"`
}

// PersonList is one page of GET /people. NextCursor is empty on the last page.
type PersonList struct {
	People     []PersonSummary `json:"people"`
	NextCursor string          `json:"next_cursor,omitempty"`
}

// RecordingMaxParts bounds how many parts one run's session recording may be
// stored as (RL-12: wardyn-rec uploads a long run's cast every 24 h, or as soon
// as half the 64 MiB per-upload cap is waiting). It exists for the reason that
// cap does: a hostile sandbox must not grow its stored recording without bound,
// and the Recordings list reads every part of a run.
//
// No existing limit bounds the honest part count: a governance profile may let
// a run have no end, and the deployment's ephemeral disk ceiling is optional.
// So this is a named ceiling, sized so an honest run does not reach it: 2048
// parts is more than five and a half years of 24 h parts, or 64 GiB of
// terminal output in 32 MiB parts, and bounds one run's store at 2048 uploads
// of at most 64 MiB (128 GiB) and a replay or list render at 2048 reads. The
// control plane refuses a higher part (413), and the proxy never forwards one.
const RecordingMaxParts = 2048
