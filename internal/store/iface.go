// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Store is the abstract persistence seam the control plane talks to. The
// default Postgres implementation is PG (query bodies in store.go); a future
// pure-Go SQLite backend satisfies this same interface unchanged.
//
// Out of scope on purpose: transactional surfaces (broker mint FOR UPDATE,
// identity revocation) need a real transaction and stay on the pool directly.
//
// SECURITY: several method groups below are deliberately part of this
// interface, not a type-asserted optional seam, because they run on a
// request-path authz decision — a test double that doesn't implement them
// would be a compile error instead of a silent fail-open degrade. Each such
// group is marked SECURITY below with its specific failure mode.
type Store interface {
	// AgentRun.
	CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error)
	GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error)
	ListRuns(ctx context.Context) ([]types.AgentRun, error)
	// CountActiveRunsBy counts one creator's non-terminal runs — the governance quota's read
	// (GovernanceLimits.MaxConcurrentRuns). Called only when an assigned profile sets a cap.
	CountActiveRunsBy(ctx context.Context, createdBy string) (int, error)
	UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error)
	UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error)
	SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error
	SetRunImage(ctx context.Context, id uuid.UUID, image string) error
	SetRunDiskMiB(ctx context.Context, id uuid.UUID, mib int) error
	SetRunAgentExecID(ctx context.Context, id uuid.UUID, execID string) error
	TouchRun(ctx context.Context, id uuid.UUID) error
	// The run-watcher lease is NOT here on purpose — see RunWatcherLeaser
	// (store_watcher.go), which follows Pager's type-assert seam.

	// RunPolicy.
	CreatePolicy(ctx context.Context, p types.RunPolicy) (types.RunPolicy, error)
	GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error)
	ListPolicies(ctx context.Context) ([]types.RunPolicy, error)
	UpdatePolicy(ctx context.Context, id uuid.UUID, name string, spec types.RunPolicySpec) (types.RunPolicy, error)
	DeletePolicy(ctx context.Context, id uuid.UUID) error

	// Workspace.
	CreateWorkspace(ctx context.Context, ws types.Workspace) (types.Workspace, error)
	GetWorkspace(ctx context.Context, id uuid.UUID) (types.Workspace, error)
	ListWorkspaces(ctx context.Context) ([]types.Workspace, error)
	// UpdateWorkspace writes the full column set, CARRYING ws.EgressEditedAt rather than stamping
	// it, or ReconcileWorkspaceEgressDecisions could re-widen a list the operator just cleared.
	// stampEgressEdit lets the ONE caller that clears the list say so, stamped by the DATABASE like
	// every other writer of that column.
	UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace, stampEgressEdit bool) (types.Workspace, error)
	// SetWorkspaceApprovedEgress replaces the approved-egress list and stamps EgressEditedAt: the
	// documented undo for an `always` decision, stopping the boot heal from re-adding a removed host.
	SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error)
	// AddWorkspaceEgressDecision records one `always`-scoped decision: on allow, host moves into
	// approved_egress (capped, deduped) and out of denied_egress; on deny the mirror. ErrConflict
	// (not ErrNotFound) when the cap refuses the write. Does NOT stamp EgressEditedAt — a decision
	// isn't an operator override of itself.
	AddWorkspaceEgressDecision(ctx context.Context, id uuid.UUID, host string, allow bool, maxApprovedEgress int) (types.Workspace, error)
	// SetWorkspaceDeniedEgress mirrors SetWorkspaceApprovedEgress for denied-egress: pass the FULL
	// desired list, replacing rather than merging.
	SetWorkspaceDeniedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error)
	SetWorkspaceLLMCred(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error)
	// SetWorkspaceOwner replaces ONLY owned_by (offboarding: reassign a departed member's workspace
	// to the operator via owner ""). Scoped and separate from UpdateWorkspace so no ordinary edit
	// can move ownership or race a concurrent async scan.
	SetWorkspaceOwner(ctx context.Context, id uuid.UUID, owner string) (types.Workspace, error)
	SetWorkspaceRequirements(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error)
	SetWorkspaceRecordResult(ctx context.Context, id uuid.UUID, taskKey string, result json.RawMessage, onlyIfStatus string) (types.Workspace, bool, error)
	ClaimWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID, expected *uuid.UUID) (types.Workspace, bool, error)
	ClearWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID) (bool, error)
	SetWorkspaceBuiltImage(ctx context.Context, id uuid.UUID, imageRef, builtHash string) (types.Workspace, error)
	// SetWorkspaceImportState advances the scan pipeline. FENCED: applies only while the import-step
	// slot still holds expectedActive; applied=false means the caller must re-read, not retry blindly.
	SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, activeRunID *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error)
	// MergeWorkspaceRequirements ADDS overlay rows atomically (jsonb ||), safe against concurrent
	// edits the full-replace SetWorkspaceRequirements would race. ErrConflict at the key cap.
	MergeWorkspaceRequirements(ctx context.Context, id uuid.UUID, add map[string]types.WorkspaceRequirement) (types.Workspace, error)
	DeleteWorkspace(ctx context.Context, id uuid.UUID) error

	// Source library (tier 1) — a repo/dir configured once, attached to many workspaces. Upsert
	// dedupes on (kind, locator, ref).
	UpsertSource(ctx context.Context, src types.Source) (types.Source, error)
	GetSource(ctx context.Context, id uuid.UUID) (types.Source, error)
	GetSourcesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.Source, error)
	ListSources(ctx context.Context) ([]types.Source, error)
	UpdateSourceConfig(ctx context.Context, id uuid.UUID, name string, reqs map[string]types.WorkspaceRequirement) (types.Source, error)
	WorkspacesAttaching(ctx context.Context, id uuid.UUID) ([]string, error)
	DeleteSource(ctx context.Context, id uuid.UUID, detach bool) error
	ClaimSourceActiveRun(ctx context.Context, id, runID uuid.UUID) error
	ClearSourceActiveRun(ctx context.Context, id, runID uuid.UUID) error
	SetSourceScanResult(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, runID uuid.UUID, seed map[string]types.WorkspaceRequirement) (types.Source, error)
	SetSourceScanResultUnfenced(ctx context.Context, id uuid.UUID, profile []byte, status types.WorkspaceStatus, seed map[string]types.WorkspaceRequirement) (types.Source, error)

	// Base-image catalog (tier 2). Upsert dedupes on (kind, image, steps); "recommended" is
	// structurally excluded (CHECK) — it is a per-workspace derived build, never a catalog row.
	UpsertBaseImage(ctx context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error)
	// UpdateBaseImageName renames a catalog row — the operator-editable counterpart to
	// UpdateSourceConfig above, deliberately NOT folded into UpsertBaseImage's identity-hit dedupe.
	UpdateBaseImageName(ctx context.Context, id uuid.UUID, name string) (types.BaseImageEntry, error)
	ListBaseImages(ctx context.Context) ([]types.BaseImageEntry, error)
	WorkspacesUsingBaseImage(ctx context.Context, id uuid.UUID) ([]string, error)
	DeleteBaseImage(ctx context.Context, id uuid.UUID, detach bool) error

	// CredentialGrant.
	CreateGrant(ctx context.Context, g types.CredentialGrant) (types.CredentialGrant, error)
	ListGrantsByRun(ctx context.Context, runID uuid.UUID) ([]types.CredentialGrant, error)

	// ApprovalRequest.
	CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error)
	GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error)
	// DecideApproval transitions state from PENDING to decision.State; returns
	// ErrAlreadyDecided if the approval is not PENDING. See types.ApprovalDecision.
	DecideApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision) (types.ApprovalRequest, error)

	// AuditEvent.
	QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error)
	QueryRecentAuditEvents(ctx context.Context, limit int) ([]types.AuditEvent, error)
	LatestAuditEventByAction(ctx context.Context, action string) (types.AuditEvent, error)

	// SiteConfig.
	GetSiteConfig(ctx context.Context) (types.SiteConfig, error)
	PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error)

	// Sandbox ref -> substrate routing (the orchestrator's RefStore seam; see
	// store_sandbox_ref.go). GetRef reports a missing row as found=false, nil.
	PutRef(ctx context.Context, ref, substrateName string) error
	GetRef(ctx context.Context, ref string) (substrateName string, found bool, err error)
	DeleteRef(ctx context.Context, ref string) error

	// Short-lived cross-process handoff row (see store_ephemeral.go): single-use WS attach tickets.
	// Consume-once, and the consumer is a single DELETE ... RETURNING, so two racing redemptions —
	// on one control plane or two — can only have one win.
	MintAttachTicket(ctx context.Context, token string, t AttachTicket, now, expiresAt time.Time) error
	ConsumeAttachTicket(ctx context.Context, token string, now time.Time) (AttachTicket, bool, error)

	// SSH gateway key registry (migration 0033, self-service via /api/v1/me/ssh-keys). AddSSHKey
	// returns ErrConflict on a taken fingerprint (the PK; one key maps to one owner). DeleteSSHKey is
	// scoped to principal (someone else's key is ErrNotFound, no existence leak). GetSSHKeyByFingerprint
	// is the gateway's unscoped auth-time lookup. RefreshSSHKeyRoles re-stamps role+role_checked_at
	// (migration 0046) from the OIDC OnLogin hook, bounding the admin-override stamp's staleness.
	AddSSHKey(ctx context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error)
	ListSSHKeysByPrincipal(ctx context.Context, principal string) ([]types.SSHPublicKey, error)
	GetSSHKeyByFingerprint(ctx context.Context, fingerprint string) (types.SSHPublicKey, error)
	DeleteSSHKey(ctx context.Context, fingerprint, principal string) error
	// DeleteSSHKeys removes one principal's keys, or all keys when principal is empty.
	DeleteSSHKeys(ctx context.Context, principal string) (int, error)
	RefreshSSHKeyRoles(ctx context.Context, principal, role string, checkedAt time.Time) error
	// RefreshAPITokenIdentity re-stamps role, user type and the group snapshot (plus its completeness
	// bit) on every unrevoked api_tokens row a principal holds, from the same OnLogin hook as
	// RefreshSSHKeyRoles — neither credential had a way to learn about a demotion or group change.
	RefreshAPITokenIdentity(ctx context.Context, principal, role, userType string, groups []string, truncated bool) error

	// Per-user API tokens (migration 0045). SECURITY: GetAPITokenByRaw is the
	// third auth branch on every authenticated route.
	//
	// CreateAPIToken/GetAPITokenByRaw hash the plaintext token internally — the raw value never
	// reaches SQL. GetAPITokenByRaw returns ErrNotFound for unknown, mismatched, revoked AND expired
	// tokens alike, so it's not an existence oracle. RevokeAPIToken is principal-scoped when non-empty
	// (self-service; someone else's id is ErrNotFound) and revokes ANY token when empty (admin).
	// TouchAPIToken is best effort — its error must never fail a request.
	CreateAPIToken(ctx context.Context, t types.APIToken, raw string) (types.APIToken, error)
	GetAPITokenByRaw(ctx context.Context, raw string) (types.APIToken, error)
	TouchAPIToken(ctx context.Context, id uuid.UUID, now time.Time) error
	ListAPITokensByPrincipal(ctx context.Context, principal string) ([]types.APIToken, error)
	ListAPITokens(ctx context.Context) ([]types.APIToken, error)
	RevokeAPIToken(ctx context.Context, id uuid.UUID, principal string, now time.Time) (types.APIToken, error)

	// Capability grants and the per-kind enforcement switch (migration 0042).
	// SECURITY: the resolver runs on the request path; the fail mode is a
	// fail-open authz gate hiding in an untyped test double.
	UpsertCapabilityGrant(ctx context.Context, g types.CapabilityGrant) (types.CapabilityGrant, error)
	DeleteCapabilityGrant(ctx context.Context, id uuid.UUID) error
	ListCapabilityGrants(ctx context.Context) ([]types.CapabilityGrant, error)
	// ListGroupDenyGrants returns the group-subject DENY rows of one kind, the only rows the
	// unresolvable-group-deny refusal can match — separate from ListCapabilityGrants so that
	// request-path check doesn't cost scale with table size.
	ListGroupDenyGrants(ctx context.Context, capability string) ([]types.CapabilityGrant, error)
	// ListCapabilityGrantsFor returns the grants that could apply to one caller: `all` rows plus
	// `user` rows naming any of users (sub AND email) plus `group` rows naming any of groups plus
	// `user_type` rows naming userType. Not filtered by capability — see the implementation.
	ListCapabilityGrantsFor(ctx context.Context, users, groups []string, userType string) ([]types.CapabilityGrant, error)
	// GetCapabilityEnforcement returns the sparse per-kind switch map; an absent key means NOT
	// enforced, the zero-config back-compat default.
	GetCapabilityEnforcement(ctx context.Context) (map[string]bool, error)
	// PutCapabilityEnforcement replaces the WHOLE map (a capability the caller omits loses its row)
	// and returns the stored result.
	PutCapabilityEnforcement(ctx context.Context, enabled map[string]bool) (map[string]bool, error)
	// ListCapabilityRestrictions returns the restricted values ("Available to: Only...", migration
	// 0081) as kind -> set of values; an absent value is not restricted. Never nil.
	ListCapabilityRestrictions(ctx context.Context) (map[string]map[string]bool, error)
	// SetCapabilityRestriction turns one value's restriction on or off (idempotent either way).
	SetCapabilityRestriction(ctx context.Context, capability, value string, restricted bool, by string) error

	// Console-managed role mappings (migration 0051): the store half of internal/auth/oidc's
	// RoleMappingSource, read once per login and merged with the chart's WARDYN_OIDC_ROLE_MAP.
	// SECURITY: on the OIDC login path; the fail mode is silently falling back
	// to env-only, WIDENING who derives admin under WARDYN_OIDC_DEFAULT_ROLE=admin.
	//
	// UpsertRoleMapping keys on the natural UNIQUE (value): re-adding an already-mapped value flips
	// its role in place, returning the EXISTING row's id on a conflict (never the candidate's).
	UpsertRoleMapping(ctx context.Context, m types.RoleMapping) (types.RoleMapping, error)
	DeleteRoleMapping(ctx context.Context, id uuid.UUID) error
	// ListRoleMappings returns every row, oldest first — the console's People
	// screen and the OIDC login-time merge's whole data need.
	ListRoleMappings(ctx context.Context) ([]types.RoleMapping, error)

	// User types (migration 0071_user_types). CreateUserType and
	// UpdateUserType return ErrConflict on a taken id or name; DeleteUserType
	// returns ErrConflict while the type is built in or still named by a
	// subject row (UserTypeReferences counts those).
	ListUserTypes(ctx context.Context) ([]types.UserType, error)
	GetUserType(ctx context.Context, id string) (types.UserType, error)
	CreateUserType(ctx context.Context, t types.UserType) (types.UserType, error)
	UpdateUserType(ctx context.Context, t types.UserType) (types.UserType, error)
	// UpdateUserTypeMetadata is UpdateUserType without the priority column.
	UpdateUserTypeMetadata(ctx context.Context, t types.UserType) (types.UserType, error)
	UserTypeReferences(ctx context.Context, id string) (int, error)
	// UserTypeTokenStamps counts the unrevoked API tokens stamped with the type.
	UserTypeTokenStamps(ctx context.Context, id string) (int, error)
	DeleteUserType(ctx context.Context, id string) error

	// Launch presets (migration 0087, store_launch_presets.go). PutLaunchPreset
	// is an upsert by name that moves the version only when the row changes.
	ListLaunchPresets(ctx context.Context) ([]types.LaunchPreset, error)
	GetLaunchPreset(ctx context.Context, name string) (types.LaunchPreset, error)
	PutLaunchPreset(ctx context.Context, p types.LaunchPreset) (types.LaunchPreset, PresetWrite, error)
	DeleteLaunchPreset(ctx context.Context, name string) (types.LaunchPreset, error)

	// Governance profiles and their subject assignments (migration 0052): the assignable ceiling
	// that replaces Config.DefaultPolicy for a principal an admin has named.
	// SECURITY: on the run-creation request path; the fail mode is falling
	// back to the deployment-wide ceiling, past the floor an admin set.
	//
	// UpsertGovernanceProfile keys on the PRIMARY KEY (a fresh id inserts, an existing one updates
	// in place, rename included) and returns ErrConflict when UNIQUE(name) rejects the write.
	// DeleteGovernanceProfile returns ErrConflict when the profile is still ASSIGNED — the ON DELETE
	// RESTRICT, so deleting a profile can never silently widen its members.
	UpsertGovernanceProfile(ctx context.Context, p types.GovernanceProfile) (types.GovernanceProfile, error)
	// WriteGovernanceProfile is the graph-aware write: one transaction under the profile-graph
	// lock, where build decides the stored row from every profile as that transaction reads them
	// (migration 0125: a profile may be composed on another). DeleteGovernanceProfile also
	// returns *ErrProfileHasChildren when another profile composes on the row.
	WriteGovernanceProfile(ctx context.Context, id uuid.UUID, build GovernanceProfileBuild) (types.GovernanceProfile, error)
	DeleteGovernanceProfile(ctx context.Context, id uuid.UUID) error
	// ListGovernanceProfiles and GetGovernanceProfileChain return RAW rows: a composed row
	// carries no authority of its own. Only internal/api/governance_compose.go calls them for
	// authority (a source guard fails any other caller); GetGovernanceProfileChain is one
	// statement, leaf first, bounded at depth 3, and ErrNotFound when the leaf is gone.
	ListGovernanceProfiles(ctx context.Context) ([]types.GovernanceProfile, error)
	GetGovernanceProfileChain(ctx context.Context, id uuid.UUID) ([]types.GovernanceProfile, error)
	// UpsertGovernanceAssignment keys on the natural UNIQUE (subject_type,
	// subject): re-assigning a subject REPOINTS its one row, returning the
	// EXISTING row's id on a conflict. ErrNotFound when profile_id names no
	// profile (the FK rejects it).
	UpsertGovernanceAssignment(ctx context.Context, a types.GovernanceAssignment) (types.GovernanceAssignment, error)
	DeleteGovernanceAssignment(ctx context.Context, id uuid.UUID) error
	ListGovernanceAssignments(ctx context.Context) ([]types.GovernanceAssignment, error)
	// Governance changes (migration 0126): a governance write held for a second human. Part of
	// this interface, not an optional seam, because a store without them would let a covered write
	// through unreviewed. See governance_changes.go for each one's contract.
	ProposeGovernanceChange(ctx context.Context, ch types.GovernanceChange, ttl time.Duration) (types.GovernanceChange, []uuid.UUID, error)
	ListGovernanceChanges(ctx context.Context, state string) ([]types.GovernanceChange, error)
	GetGovernanceChange(ctx context.Context, id uuid.UUID) (types.GovernanceChange, error)
	DecideGovernanceChange(ctx context.Context, id uuid.UUID, d GovernanceDecision, fn GovernanceDecideFunc) (types.GovernanceChange, error)
	// DryRunGovernance runs fn on a transaction that is always rolled back.
	DryRunGovernance(ctx context.Context, fn func(q Querier) error) error
	// ResolveGovernanceProfile returns THE ONE profile that applies to a caller — user > group >
	// user_type > all, sub over email within the user tier, then priority DESC and name ASC — as a
	// single indexed read whose ORDER BY is the whole precedence rule. ErrNotFound means "no
	// assignment matched", read as the deployment ceiling. userSubjects must arrive in the caller's
	// own precedence order since the query matches by POSITION.
	//
	// Also returns WHICH TIER matched, load-bearing not informational: on a
	// stale/truncated group snapshot the caller must tell a user-tier winner
	// (serve it) from an all-tier one (refuse), indistinguishable from the
	// profile alone. HasGroupTierAssignments below is that separate read.
	ResolveGovernanceProfile(ctx context.Context, userSubjects, groups []string, userType string) (*types.GovernanceProfile, types.CapabilitySubjectType, error)
	// HasGroupTierAssignments gates the stale/truncated group-snapshot refusal:
	// with no group-tier row there is nothing an unknown group could have
	// matched, so a nil or truncated snapshot must fall through rather than
	// refuse (see the implementation).
	HasGroupTierAssignments(ctx context.Context) (bool, error)

	// User drives and their subject grants (migration 0054): admin-registered per-user storage a
	// member may mount into a run, and the rows allocating one to a user, group, or everyone.
	// SECURITY: same reason as the governance resolver above.
	//
	// UpsertUserDrive keys on the PRIMARY KEY (fresh id inserts, existing updates in place, rename
	// included) and returns ErrConflict when UNIQUE(name) rejects the write. DeleteUserDrive returns
	// ErrConflict while the drive is still ALLOCATED (ON DELETE RESTRICT).
	//
	// refuseIfAllocated is a PRECONDITION: the write applies only while the
	// drive has no allocations (ErrDriveAllocated otherwise) — the API's
	// re-home guard re-asserts, inside the writing statement, the read
	// decision that permitted the write.
	UpsertUserDrive(ctx context.Context, d types.UserDrive, refuseIfAllocated bool) (types.UserDrive, error)
	GetUserDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error)
	DeleteUserDrive(ctx context.Context, id uuid.UUID) error
	// ListUserDrives returns every drive by name WITH its grant count — what makes the console's
	// delete affordance honest, since a drive with grants answers 409.
	ListUserDrives(ctx context.Context) ([]types.UserDriveListItem, error)
	// UpsertUserDriveGrant keys on the natural UNIQUE (subject_type, subject): re-allocating a
	// subject REPOINTS its one row, returning the EXISTING row's id on a conflict. ErrNotFound when
	// drive_id names no drive (FK). ErrConflict when ANOTHER subject already holds this drive with
	// the same home_override — a directory name is one person's, and on a managed drive an override
	// is the last way left to point two people at one object Wardyn creates.
	//
	// homeOverrideStated is the REQUEST's tri-state, not the row's: false means the caller never
	// mentioned home_override, and a repoint that never mentioned it must not CLEAR one (see the
	// guard in PG's statement) — that same ErrConflict, distinguished from the uniqueness one purely
	// by this argument (a stated override disables the re-home guard; an unstated one is empty).
	UpsertUserDriveGrant(ctx context.Context, g types.UserDriveGrant, homeOverrideStated bool) (types.UserDriveGrant, error)
	// DeleteUserDriveGrant RETURNS the row it removed (ErrNotFound when none
	// matched): the delete's own audit row has to name the subject that was
	// de-allocated, and by then it is gone.
	DeleteUserDriveGrant(ctx context.Context, id uuid.UUID) (types.UserDriveGrant, error)
	ListUserDriveGrants(ctx context.Context) ([]types.UserDriveGrant, error)
	// ResolveUserDrive mirrors ResolveGovernanceProfile's precedence and
	// stale-snapshot reasoning. DISABLED grants stay IN the query: one that
	// wins its tier comes back Enabled false, rendered PAUSED rather than
	// falling through (a pause must never widen a member onto an unchosen
	// drive). Returns the winning GRANT beside the drive since every override
	// lives on the binding row. ErrNotFound means "no drive".
	ResolveUserDrive(ctx context.Context, userSubjects, groups []string, userType string) (
		*types.UserDrive, *types.UserDriveGrant, types.CapabilitySubjectType, error)
	// HasGroupTierDriveGrants gates the stale/truncated group-snapshot refusal:
	// with no group-tier row there is nothing an unknown group could have
	// matched, so a nil or truncated snapshot must fall through rather than
	// refuse (see the implementation).
	HasGroupTierDriveGrants(ctx context.Context) (bool, error)

	// Ping proves the store is actually reachable, not just constructed — the
	// /readyz readiness probe's one call. A live TCP connect with no working
	// query would otherwise read as healthy forever.
	Ping(ctx context.Context) error
}

// PG is the Postgres-backed Store: its methods (defined in store.go) hold the
// query bodies directly, so there is exactly one implementation of each query.
type PG struct {
	Pool *pgxpool.Pool

	// Now is THE APP CLOCK, letting a test run one that disagrees with the
	// database's. Nil means time.Now. Every stamp this store writes belongs on
	// the DATABASE's clock; the app clock only measures an ELAPSED TIME (a
	// difference of two readings, carrying no skew) that a statement then
	// subtracts from the database's own now(). See db.AppClockAgeSQL.
	Now func() time.Time
}

// now reads the app clock. BOTH readings that make an age must come from here.
func (s PG) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// NewPG returns a PG Store over pool.
func NewPG(pool *pgxpool.Pool) PG { return PG{Pool: pool} }

// Compile-time assertion: PG satisfies Store.
var _ Store = PG{}
