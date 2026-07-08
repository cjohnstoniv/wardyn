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

// Store is the abstract persistence seam the control plane talks to. It mirrors
// the free CRUD/read functions in this package one-for-one, dropping the
// explicit *pgxpool.Pool parameter — the concrete implementation carries its own
// handle. The default Postgres implementation is PG (a thin veneer over the free
// functions, which the *_pg_test.go tests still call directly); a future pure-Go
// SQLite backend will satisfy this same interface without touching the API layer.
//
// Out of scope on purpose: the transactional surfaces (broker mint FOR UPDATE,
// identity revocation) need a real transaction rather than a single-call store
// and stay on the pool directly.
type Store interface {
	// AgentRun.
	CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error)
	GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error)
	ListRuns(ctx context.Context) ([]types.AgentRun, error)
	UpdateRunState(ctx context.Context, id uuid.UUID, state types.RunState) error
	UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error)
	UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error)
	SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error
	TouchRun(ctx context.Context, id uuid.UUID) error

	// RunPolicy.
	CreatePolicy(ctx context.Context, p types.RunPolicy) (types.RunPolicy, error)
	GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error)
	ListPolicies(ctx context.Context) ([]types.RunPolicy, error)
	UpdatePolicy(ctx context.Context, id uuid.UUID, name string, spec types.RunPolicySpec) (types.RunPolicy, error)
	DeletePolicy(ctx context.Context, id uuid.UUID) error

	// Workspace.
	CreateWorkspace(ctx context.Context, ws types.Workspace) (types.Workspace, error)
	GetWorkspace(ctx context.Context, id uuid.UUID) (types.Workspace, error)
	GetWorkspaceBySource(ctx context.Context, kind types.WorkspaceKind, source string) (types.Workspace, error)
	ListWorkspaces(ctx context.Context) ([]types.Workspace, error)
	UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace) (types.Workspace, error)
	SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error)
	SetWorkspaceSetupCommands(ctx context.Context, id uuid.UUID, cmds json.RawMessage) (types.Workspace, error)
	SetWorkspaceRecordResult(ctx context.Context, id uuid.UUID, taskKey string, result json.RawMessage, onlyIfStatus string) (types.Workspace, bool, error)
	ClaimWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID, expected *uuid.UUID) (types.Workspace, bool, error)
	ClearWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID) (bool, error)
	SetWorkspaceBuiltImage(ctx context.Context, id uuid.UUID, imageRef, builtHash string) (types.Workspace, error)
	SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, activeRunID *uuid.UUID, verifyResult json.RawMessage, verifiedHash string, verifiedAt *time.Time) (types.Workspace, error)
	DeleteWorkspace(ctx context.Context, id uuid.UUID) error

	// CredentialGrant.
	CreateGrant(ctx context.Context, g types.CredentialGrant) (types.CredentialGrant, error)
	GetGrant(ctx context.Context, id uuid.UUID) (types.CredentialGrant, error)
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
}

// PG is the Postgres-backed Store. It is a thin adapter: every method delegates
// to the corresponding free function with its embedded pool, so there is exactly
// one implementation of each query and the free functions (used directly by the
// *_pg_test.go suite) remain the single source of truth.
type PG struct {
	Pool *pgxpool.Pool
}

// NewPG returns a PG Store over pool.
func NewPG(pool *pgxpool.Pool) PG { return PG{Pool: pool} }

// Compile-time assertion: PG satisfies Store.
var _ Store = PG{}

// ─── AgentRun ────────────────────────────────────────────────────────────────

func (s PG) CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
	return CreateRun(ctx, s.Pool, r)
}
func (s PG) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	return GetRun(ctx, s.Pool, id)
}
func (s PG) ListRuns(ctx context.Context) ([]types.AgentRun, error) {
	return ListRuns(ctx, s.Pool)
}
func (s PG) UpdateRunState(ctx context.Context, id uuid.UUID, state types.RunState) error {
	return UpdateRunState(ctx, s.Pool, id, state)
}
func (s PG) UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error) {
	return UpdateRunStateIf(ctx, s.Pool, id, fromState, toState)
}
func (s PG) UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error) {
	return UpdateRunStateIfIdle(ctx, s.Pool, id, fromState, toState, notAfter)
}
func (s PG) SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error {
	return SetSandboxRef(ctx, s.Pool, id, ref)
}
func (s PG) TouchRun(ctx context.Context, id uuid.UUID) error {
	return TouchRun(ctx, s.Pool, id)
}

// ─── RunPolicy ───────────────────────────────────────────────────────────────

func (s PG) CreatePolicy(ctx context.Context, p types.RunPolicy) (types.RunPolicy, error) {
	return CreatePolicy(ctx, s.Pool, p)
}
func (s PG) GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error) {
	return GetPolicy(ctx, s.Pool, id)
}
func (s PG) ListPolicies(ctx context.Context) ([]types.RunPolicy, error) {
	return ListPolicies(ctx, s.Pool)
}
func (s PG) UpdatePolicy(ctx context.Context, id uuid.UUID, name string, spec types.RunPolicySpec) (types.RunPolicy, error) {
	return UpdatePolicy(ctx, s.Pool, id, name, spec)
}
func (s PG) DeletePolicy(ctx context.Context, id uuid.UUID) error {
	return DeletePolicy(ctx, s.Pool, id)
}

// ─── Workspace ───────────────────────────────────────────────────────────────

func (s PG) CreateWorkspace(ctx context.Context, ws types.Workspace) (types.Workspace, error) {
	return CreateWorkspace(ctx, s.Pool, ws)
}
func (s PG) GetWorkspace(ctx context.Context, id uuid.UUID) (types.Workspace, error) {
	return GetWorkspace(ctx, s.Pool, id)
}
func (s PG) GetWorkspaceBySource(ctx context.Context, kind types.WorkspaceKind, source string) (types.Workspace, error) {
	return GetWorkspaceBySource(ctx, s.Pool, kind, source)
}
func (s PG) ListWorkspaces(ctx context.Context) ([]types.Workspace, error) {
	return ListWorkspaces(ctx, s.Pool)
}
func (s PG) UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	return UpdateWorkspace(ctx, s.Pool, id, ws)
}
func (s PG) SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return SetWorkspaceApprovedEgress(ctx, s.Pool, id, domains)
}
func (s PG) SetWorkspaceSetupCommands(ctx context.Context, id uuid.UUID, cmds json.RawMessage) (types.Workspace, error) {
	return SetWorkspaceSetupCommands(ctx, s.Pool, id, cmds)
}
func (s PG) SetWorkspaceRecordResult(ctx context.Context, id uuid.UUID, taskKey string, result json.RawMessage, onlyIfStatus string) (types.Workspace, bool, error) {
	return SetWorkspaceRecordResult(ctx, s.Pool, id, taskKey, result, onlyIfStatus)
}
func (s PG) ClaimWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID, expected *uuid.UUID) (types.Workspace, bool, error) {
	return ClaimWorkspaceActiveRun(ctx, s.Pool, id, runID, expected)
}
func (s PG) ClearWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID) (bool, error) {
	return ClearWorkspaceActiveRun(ctx, s.Pool, id, runID)
}
func (s PG) SetWorkspaceBuiltImage(ctx context.Context, id uuid.UUID, imageRef, builtHash string) (types.Workspace, error) {
	return SetWorkspaceBuiltImage(ctx, s.Pool, id, imageRef, builtHash)
}
func (s PG) SetWorkspaceImportState(ctx context.Context, id uuid.UUID, status types.WorkspaceStatus, activeRunID *uuid.UUID, verifyResult json.RawMessage, verifiedHash string, verifiedAt *time.Time) (types.Workspace, error) {
	return SetWorkspaceImportState(ctx, s.Pool, id, status, activeRunID, verifyResult, verifiedHash, verifiedAt)
}
func (s PG) DeleteWorkspace(ctx context.Context, id uuid.UUID) error {
	return DeleteWorkspace(ctx, s.Pool, id)
}

// ─── CredentialGrant ─────────────────────────────────────────────────────────

func (s PG) CreateGrant(ctx context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	return CreateGrant(ctx, s.Pool, g)
}
func (s PG) GetGrant(ctx context.Context, id uuid.UUID) (types.CredentialGrant, error) {
	return GetGrant(ctx, s.Pool, id)
}
func (s PG) ListGrantsByRun(ctx context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	return ListGrantsByRun(ctx, s.Pool, runID)
}

// ─── ApprovalRequest ─────────────────────────────────────────────────────────

func (s PG) CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	return CreateApproval(ctx, s.Pool, a)
}
func (s PG) GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	return GetApproval(ctx, s.Pool, id)
}
func (s PG) ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error) {
	return ListApprovals(ctx, s.Pool, stateFilter)
}
func (s PG) DecideApproval(ctx context.Context, id uuid.UUID, state types.ApprovalState, decidedBy, reason string) (types.ApprovalRequest, error) {
	return DecideApproval(ctx, s.Pool, id, state, decidedBy, reason)
}

// ─── AuditEvent ──────────────────────────────────────────────────────────────

func (s PG) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	return QueryAuditEvents(ctx, s.Pool, runID, limit)
}
func (s PG) QueryRecentAuditEvents(ctx context.Context, limit int) ([]types.AuditEvent, error) {
	return QueryRecentAuditEvents(ctx, s.Pool, limit)
}
func (s PG) LatestAuditEventByAction(ctx context.Context, action string) (types.AuditEvent, error) {
	return LatestAuditEventByAction(ctx, s.Pool, action)
}

// ─── SiteConfig ──────────────────────────────────────────────────────────────

func (s PG) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	return GetSiteConfig(ctx, s.Pool)
}
func (s PG) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	return PutSiteConfig(ctx, s.Pool, cfg)
}
