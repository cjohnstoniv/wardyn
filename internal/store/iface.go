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
	SetWorkspaceScanResult(ctx context.Context, id uuid.UUID, profile json.RawMessage, runID uuid.UUID) (types.Workspace, bool, error)
	DeleteWorkspace(ctx context.Context, id uuid.UUID) error

	// CredentialGrant.
	CreateGrant(ctx context.Context, g types.CredentialGrant) (types.CredentialGrant, error)
	ListGrantsByRun(ctx context.Context, runID uuid.UUID) ([]types.CredentialGrant, error)

	// ApprovalRequest.
	CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error)
	GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error)
	ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error)
	DecideApproval(ctx context.Context, id uuid.UUID, state types.ApprovalState, decidedBy, reason string) (types.ApprovalRequest, error)

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

	// Short-lived cross-process handoff rows (see store_ephemeral.go): single-use
	// WS attach tickets and compose-run proposal uploads. Both are consume-once,
	// and the consumer is a single DELETE ... RETURNING, so two racing
	// redemptions — on one control plane or two — can only have one win.
	MintAttachTicket(ctx context.Context, token string, t AttachTicket, now, expiresAt time.Time) error
	ConsumeAttachTicket(ctx context.Context, token string, now time.Time) (AttachTicket, bool, error)
	PutComposeResult(ctx context.Context, runID uuid.UUID, payload []byte) error
	TakeComposeResult(ctx context.Context, runID uuid.UUID) ([]byte, bool, error)
	DiscardComposeResult(ctx context.Context, runID uuid.UUID) error
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
