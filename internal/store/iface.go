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
// default Postgres implementation is PG, whose methods hold the query bodies
// directly (no pool param — the receiver carries its own handle); a future
// pure-Go SQLite backend will satisfy this same interface without touching
// the API layer.
//
// Out of scope on purpose: the transactional surfaces (broker mint FOR UPDATE,
// identity revocation) need a real transaction rather than a single-call store
// and stay on the pool directly.
type Store interface {
	// AgentRun.
	CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error)
	GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error)
	ListRuns(ctx context.Context) ([]types.AgentRun, error)
	// CountActiveRunsBy counts one creator's non-terminal runs — the
	// governance quota's read (GovernanceLimits.MaxConcurrentRuns). Called
	// ONLY when an assigned profile actually sets a cap, so a deployment with
	// no governance assignments never reaches it.
	CountActiveRunsBy(ctx context.Context, createdBy string) (int, error)
	UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error)
	UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error)
	SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error
	SetRunImage(ctx context.Context, id uuid.UUID, image string) error
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
	UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace) (types.Workspace, error)
	// SetWorkspaceApprovedEgress replaces the operator-owned approved-egress
	// list and stamps Workspace.EgressEditedAt: this PUT is the documented undo
	// for an `always` decision, and that stamp is what stops
	// ReconcileWorkspaceEgressDecisions putting a removed host back at the next
	// boot. An implementation that skips it re-opens that resurrection.
	SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error)
	// AddWorkspaceEgressDecision records one `always`-scoped egress decision:
	// on allow, host is added to approved_egress (capped at maxApprovedEgress,
	// deduped) and removed from denied_egress; on deny the mirror. Returns
	// ErrConflict (not ErrNotFound) when id exists but the cap refused the
	// write. It does NOT stamp EgressEditedAt — a decision is not an operator
	// override of itself, and stamping here would make the boot heal suppress
	// its own future re-applies. See store.go for the full contract.
	AddWorkspaceEgressDecision(ctx context.Context, id uuid.UUID, host string, allow bool, maxApprovedEgress int) (types.Workspace, error)
	// SetWorkspaceDeniedEgress is SetWorkspaceApprovedEgress's mirror for the
	// operator-owned denied-egress list (Phase 4 revocation PUT): pass the
	// FULL desired list, replacing rather than merging, and stamp
	// EgressEditedAt for the same reason.
	SetWorkspaceDeniedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error)
	SetWorkspaceLLMCred(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error)
	// SetWorkspaceOwner replaces ONLY the owned_by column (plus updated_at).
	// The offboarding path (design decision O6): an admin reassigns a departed
	// member's workspace to the operator by setting owner "". Scoped for the
	// same reason SetWorkspaceLLMCred is — it must never replay a stale
	// snapshot over a concurrently-persisted async scan — and separate from
	// UpdateWorkspace on purpose: the full-row update deliberately does not
	// carry owned_by, so no ordinary edit can move ownership.
	SetWorkspaceOwner(ctx context.Context, id uuid.UUID, owner string) (types.Workspace, error)
	SetWorkspaceRequirements(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error)
	SetWorkspaceRecordResult(ctx context.Context, id uuid.UUID, taskKey string, result json.RawMessage, onlyIfStatus string) (types.Workspace, bool, error)
	ClaimWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID, expected *uuid.UUID) (types.Workspace, bool, error)
	ClearWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID) (bool, error)
	SetWorkspaceBuiltImage(ctx context.Context, id uuid.UUID, imageRef, builtHash string) (types.Workspace, error)
	// SetWorkspaceImportState advances the scan pipeline. FENCED: the write
	// applies only while the import-step slot still holds expectedActive (nil =
	// expected empty); applied=false means it moved and the caller must re-read
	// instead of retrying blindly.
	SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, activeRunID *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error)
	// MergeWorkspaceRequirements ADDS overlay rows atomically (jsonb ||) — the
	// verify loop's approve-writes-the-row-now, safe against concurrent edits
	// the full-replace SetWorkspaceRequirements would race. ErrConflict at the
	// key cap.
	MergeWorkspaceRequirements(ctx context.Context, id uuid.UUID, add map[string]types.WorkspaceRequirement) (types.Workspace, error)
	DeleteWorkspace(ctx context.Context, id uuid.UUID) error

	// Source library (tier 1) — a repo/dir configured once, attached to many
	// workspaces. Upsert dedupes on (kind, locator, ref); the scan lifecycle
	// (fence, result) mirrors the workspace's own pre-split shape.
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

	// Base-image catalog (tier 2). Upsert dedupes on (kind, image, steps);
	// "recommended" is structurally excluded (CHECK) — it is a per-workspace
	// derived build, never a catalog row.
	UpsertBaseImage(ctx context.Context, b types.BaseImageEntry) (types.BaseImageEntry, error)
	// UpdateBaseImageName renames a catalog row (W7-S1-3) — the operator-editable
	// counterpart to UpdateSourceConfig above, deliberately NOT folded into
	// UpsertBaseImage's identity-hit dedupe. See both doc comments.
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

	// Short-lived cross-process handoff row (see store_ephemeral.go): single-use
	// WS attach tickets. Consume-once, and the consumer is a single
	// DELETE ... RETURNING, so two racing redemptions — on one control plane or
	// two — can only have one win.
	MintAttachTicket(ctx context.Context, token string, t AttachTicket, now, expiresAt time.Time) error
	ConsumeAttachTicket(ctx context.Context, token string, now time.Time) (AttachTicket, bool, error)

	// SSH gateway key registry (migration 0033, self-service via
	// /api/v1/me/ssh-keys). AddSSHKey returns ErrConflict when the fingerprint
	// (the PK) is already registered — by this principal or another; a given
	// key material maps to exactly one owner. DeleteSSHKey is scoped to
	// principal (an attempted delete of someone else's key is ErrNotFound, not
	// a distinguishable 403 — no existence leak). GetSSHKeyByFingerprint is the
	// gateway's auth-time lookup (unscoped: the caller has not authenticated
	// yet, that IS what this call resolves). RefreshSSHKeyRoles re-stamps
	// role+role_checked_at (migration 0046) on every key owned by principal —
	// the OIDC callback's OnLogin hook, bounding the admin-override stamp's
	// staleness instead of leaving it fixed at registration time forever.
	AddSSHKey(ctx context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error)
	ListSSHKeysByPrincipal(ctx context.Context, principal string) ([]types.SSHPublicKey, error)
	GetSSHKeyByFingerprint(ctx context.Context, fingerprint string) (types.SSHPublicKey, error)
	DeleteSSHKey(ctx context.Context, fingerprint, principal string) error
	RefreshSSHKeyRoles(ctx context.Context, principal, role string, checkedAt time.Time) error

	// Per-user API tokens (migration 0045, self-service via /api/v1/me/tokens
	// and admin-wide via /api/v1/tokens). These ARE part of Store for the same
	// reason the capability methods below are: GetAPITokenByRaw runs on the
	// REQUEST PATH of every route in the authenticated group (it is the third
	// auth branch — see apiTokenAuth in internal/api/apitokens.go), so a
	// store that cannot answer it must be a COMPILE error, never a
	// degrade-to-allow type-assert hiding in a test double.
	//
	// CreateAPIToken and GetAPITokenByRaw take the PLAINTEXT token and hash it
	// internally — the raw value never reaches SQL. GetAPITokenByRaw is the
	// auth-time lookup (unscoped: the caller has not authenticated yet, that IS
	// what this call resolves) and returns ErrNotFound for unknown, mismatched
	// AND revoked tokens alike, so the boundary is not an existence oracle.
	// RevokeAPIToken is principal-scoped when principal is non-empty (the
	// self-service path; someone else's id is ErrNotFound, not a
	// distinguishable 403) and revokes ANY token when it is empty (the admin
	// path). TouchAPIToken is best effort — its error must never fail a request.
	CreateAPIToken(ctx context.Context, t types.APIToken, raw string) (types.APIToken, error)
	GetAPITokenByRaw(ctx context.Context, raw string) (types.APIToken, error)
	TouchAPIToken(ctx context.Context, id uuid.UUID, now time.Time) error
	ListAPITokensByPrincipal(ctx context.Context, principal string) ([]types.APIToken, error)
	ListAPITokens(ctx context.Context) ([]types.APIToken, error)
	RevokeAPIToken(ctx context.Context, id uuid.UUID, principal string, now time.Time) (types.APIToken, error)

	// Capability grants and the per-kind enforcement switch (migration 0042,
	// store_capabilities.go). These ARE part of Store — unlike RunLayoutStore /
	// Pager, which stayed out of it precisely so an embedded-nil test double
	// would not route to a nil interface — because the resolver runs on the
	// REQUEST PATH of routes every one of those doubles already serves. A
	// type-assert-and-degrade seam there would mean "this fake does not
	// implement capabilities, therefore allow", which is a fail-OPEN authz
	// gate hiding in a test-only branch. Widening Store makes a store that
	// cannot answer a permission question a COMPILE error instead.
	UpsertCapabilityGrant(ctx context.Context, g types.CapabilityGrant) (types.CapabilityGrant, error)
	DeleteCapabilityGrant(ctx context.Context, id uuid.UUID) error
	ListCapabilityGrants(ctx context.Context) ([]types.CapabilityGrant, error)
	// ListCapabilityGrantsFor returns the grants that could apply to one caller:
	// the `all` rows plus the `user` rows naming any of users (sub AND email)
	// plus the `group` rows naming any of groups. Not filtered by capability —
	// see the implementation's doc comment.
	ListCapabilityGrantsFor(ctx context.Context, users, groups []string) ([]types.CapabilityGrant, error)
	// GetCapabilityEnforcement returns the sparse per-kind switch map; an absent
	// key means NOT enforced, which is the zero-config back-compat default.
	GetCapabilityEnforcement(ctx context.Context) (map[string]bool, error)
	// PutCapabilityEnforcement replaces the WHOLE map (a capability the caller
	// omits loses its row) and returns the stored result.
	PutCapabilityEnforcement(ctx context.Context, enabled map[string]bool) (map[string]bool, error)

	// Console-managed role mappings (migration 0051, store_role_mappings.go):
	// the store half of internal/auth/oidc's RoleMappingSource, read once per
	// login (via the cmd/wardynd adapter that bridges store -> oidc, mirroring
	// SessionRevocations) and merged with the chart's WARDYN_OIDC_ROLE_MAP. ARE
	// part of Store for the identical reason the capability methods above are:
	// a store that cannot answer this runs on the OIDC login path, and a
	// type-assert-and-degrade seam there would mean "this fake does not
	// implement role mappings, therefore fall back to env-only", which is a
	// silent WIDENING of who derives admin under WARDYN_OIDC_DEFAULT_ROLE=admin
	// — not something a nil interface should be able to decide by omission.
	//
	// UpsertRoleMapping keys on the natural UNIQUE (value): re-adding an
	// already-mapped value flips its role in place, returning the EXISTING
	// row's id on a conflict (never the candidate's), the same contract
	// UpsertCapabilityGrant follows.
	UpsertRoleMapping(ctx context.Context, m types.RoleMapping) (types.RoleMapping, error)
	DeleteRoleMapping(ctx context.Context, id uuid.UUID) error
	// ListRoleMappings returns every row, oldest first — the console's People
	// screen and the OIDC login-time merge's whole data need.
	ListRoleMappings(ctx context.Context) ([]types.RoleMapping, error)

	// Governance profiles and their subject assignments (migration 0052,
	// governance.go): the assignable ceiling that replaces Config.DefaultPolicy
	// for a principal an admin has named. ARE part of Store, for the third time
	// and the strongest instance of the same reason: ResolveGovernanceProfile
	// runs on the REQUEST PATH of run creation, and a type-assert-and-degrade
	// seam there would mean "this store does not implement governance,
	// therefore use the DEPLOYMENT-WIDE ceiling" — which is a silent WIDENING
	// back to exactly the floor an admin declared too loose for this principal.
	// That is not a decision a nil interface gets to make by omission, so a
	// store that cannot answer it is a COMPILE error.
	//
	// UpsertGovernanceProfile keys on the PRIMARY KEY (a fresh id inserts, an
	// existing one updates in place, rename included) and returns ErrConflict
	// when UNIQUE(name) rejects the write. DeleteGovernanceProfile returns
	// ErrConflict when the profile is still ASSIGNED — the ON DELETE RESTRICT,
	// which exists so deleting a profile can never silently widen its members.
	UpsertGovernanceProfile(ctx context.Context, p types.GovernanceProfile) (types.GovernanceProfile, error)
	DeleteGovernanceProfile(ctx context.Context, id uuid.UUID) error
	ListGovernanceProfiles(ctx context.Context) ([]types.GovernanceProfile, error)
	// UpsertGovernanceAssignment keys on the natural UNIQUE (subject_type,
	// subject): re-assigning a subject REPOINTS its one row, returning the
	// EXISTING row's id on a conflict. ErrNotFound when profile_id names no
	// profile (the FK rejects it).
	UpsertGovernanceAssignment(ctx context.Context, a types.GovernanceAssignment) (types.GovernanceAssignment, error)
	DeleteGovernanceAssignment(ctx context.Context, id uuid.UUID) error
	ListGovernanceAssignments(ctx context.Context) ([]types.GovernanceAssignment, error)
	// ResolveGovernanceProfile returns THE ONE profile that applies to a caller
	// — user > group > all, sub over email within the user tier, then priority
	// DESC and name ASC — as a single indexed read whose ORDER BY IS the whole
	// precedence rule. ErrNotFound means "no assignment matched", which the
	// caller reads as the deployment ceiling. userSubjects must arrive in the
	// caller's own precedence order (capabilitySubjects' [sub, email]); the
	// query encodes match POSITION, not identity kind.
	//
	// It also returns WHICH TIER matched, and that second value is load-bearing
	// rather than informational: on a stale or truncated group snapshot the
	// caller must resolve with NO groups, and it then has to tell a user-tier
	// winner (serve it — an explicitly named principal is never locked out by a
	// snapshot problem) from an all-tier winner (refuse — a group row could
	// have outranked it). Those two are indistinguishable from the profile
	// alone, including when both tiers name the SAME profile id, and
	// re-deriving the tier in Go from the subjects would be a second copy of
	// the ORDER BY above — the dual-matcher drift this file refuses elsewhere.
	//
	// The group-tier existence leg stays a SEPARATE read (below) and is not
	// folded into this statement: the case that needs it most is the one where
	// this query matches NOTHING, and a zero-row result carries no EXISTS
	// column with it.
	ResolveGovernanceProfile(ctx context.Context, userSubjects, groups []string) (*types.GovernanceProfile, types.CapabilitySubjectType, error)
	// HasGroupTierAssignments gates the stale/truncated group-snapshot refusal:
	// with no group-tier row there is nothing an unknown group could have
	// matched, so a nil or truncated snapshot must fall through rather than
	// refuse (see the implementation).
	HasGroupTierAssignments(ctx context.Context) (bool, error)

	// User drives and their subject grants (migration 0054, user_drives.go):
	// the admin-registered per-user storage a member may mount into a run, and
	// the rows allocating one to a user, a group, or everyone. ARE part of
	// Store, for the same reason the governance resolver is: ResolveUserDrive
	// runs on the REQUEST PATH of run creation, and a type-assert-and-degrade
	// seam there would mean "this store does not implement drives, therefore
	// mount nothing" — which is a silent failure of a member's data to appear
	// rather than a refusal they can act on. A store that cannot answer it is a
	// COMPILE error.
	//
	// UpsertUserDrive keys on the PRIMARY KEY (a fresh id inserts, an existing
	// one updates in place, rename included) and returns ErrConflict when
	// UNIQUE(name) rejects the write. DeleteUserDrive returns ErrConflict when
	// the drive is still ALLOCATED — the ON DELETE RESTRICT, which exists so
	// deleting a drive can never orphan the directories its grants named.
	UpsertUserDrive(ctx context.Context, d types.UserDrive) (types.UserDrive, error)
	GetUserDrive(ctx context.Context, id uuid.UUID) (types.UserDrive, error)
	DeleteUserDrive(ctx context.Context, id uuid.UUID) error
	// ListUserDrives returns every drive by name WITH its grant count — the
	// count is what makes the console's delete affordance honest, since a
	// drive with grants answers 409.
	ListUserDrives(ctx context.Context) ([]types.UserDriveListItem, error)
	// UpsertUserDriveGrant keys on the natural UNIQUE (subject_type, subject):
	// re-allocating a subject REPOINTS its one row, returning the EXISTING
	// row's id on a conflict. ErrNotFound when drive_id names no drive (the FK
	// rejects it). ErrConflict when ANOTHER subject already holds this drive
	// with the same home_override — a directory name is one person's, which is
	// why a group row may not carry one, and on a managed drive it is the last
	// way left to point two people at one object Wardyn creates.
	UpsertUserDriveGrant(ctx context.Context, g types.UserDriveGrant) (types.UserDriveGrant, error)
	// DeleteUserDriveGrant RETURNS the row it removed (ErrNotFound when none
	// matched): the delete's own audit row has to name the subject that was
	// de-allocated, and by then it is gone.
	DeleteUserDriveGrant(ctx context.Context, id uuid.UUID) (types.UserDriveGrant, error)
	ListUserDriveGrants(ctx context.Context) ([]types.UserDriveGrant, error)
	// ResolveUserDrive returns THE ONE drive that applies to a caller — user >
	// group > all, sub over email within the user tier, then priority DESC and
	// the drive's name ASC — as a single indexed read whose ORDER BY IS the
	// whole precedence rule, the same one ResolveGovernanceProfile carries.
	// DISABLED grants are IN the query: one that wins its tier comes back with
	// Enabled false, which the caller renders as PAUSED rather than falling
	// through to the wider row beneath it (DESIGN §2.2 — a pause must never
	// widen a member onto a drive no admin chose for them). ErrNotFound means
	// "no grant matched at all", which the caller reads as "no drive".
	//
	// It returns the winning GRANT beside the drive because every override the
	// resolution needs (size, writable, home) is a column on the binding row —
	// and the tier it returns is that grant's own subject_type, so unlike the
	// governance resolver there is nothing here that can disagree with the row
	// it came from. The tier is still load-bearing: on a stale or truncated
	// group snapshot the caller must resolve with NO groups and then tell a
	// user-tier winner (serve it) from an all-tier one (refuse — a group row
	// could have outranked it).
	ResolveUserDrive(ctx context.Context, userSubjects, groups []string) (
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
}

// NewPG returns a PG Store over pool.
func NewPG(pool *pgxpool.Pool) PG { return PG{Pool: pool} }

// Compile-time assertion: PG satisfies Store.
var _ Store = PG{}
