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
	SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error)
	// AddWorkspaceEgressDecision records one `always`-scoped egress decision:
	// on allow, host is added to approved_egress (capped at maxApprovedEgress,
	// deduped) and removed from denied_egress; on deny the mirror. Returns
	// ErrConflict (not ErrNotFound) when id exists but the cap refused the
	// write. See store.go for the full contract.
	AddWorkspaceEgressDecision(ctx context.Context, id uuid.UUID, host string, allow bool, maxApprovedEgress int) (types.Workspace, error)
	// SetWorkspaceDeniedEgress is SetWorkspaceApprovedEgress's mirror for the
	// operator-owned denied-egress list (Phase 4 revocation PUT): pass the
	// FULL desired list, replacing rather than merging.
	SetWorkspaceDeniedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error)
	SetWorkspaceLLMCred(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error)
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
	GetBaseImagesByIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]types.BaseImageEntry, error)
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
	GetBaseImage(ctx context.Context, id uuid.UUID) (types.BaseImageEntry, error)
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
	// yet, that IS what this call resolves).
	AddSSHKey(ctx context.Context, k types.SSHPublicKey) (types.SSHPublicKey, error)
	ListSSHKeysByPrincipal(ctx context.Context, principal string) ([]types.SSHPublicKey, error)
	GetSSHKeyByFingerprint(ctx context.Context, fingerprint string) (types.SSHPublicKey, error)
	DeleteSSHKey(ctx context.Context, fingerprint, principal string) error

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
