// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package store provides typed CRUD over the Wardyn schema using pgx/v5.
// All writes are serialised through pgxpool; callers supply contexts with
// deadlines. Most operations are methods on PG (see iface.go); InsertAuditEvent
// stays a free function taking the pool explicitly since it predates a Store
// value in the audit.Recorder wiring.
//
// Naming conventions:
//   - Create* inserts and returns the full hydrated row.
//   - Get* fetches by primary key; returns ErrNotFound when absent.
//   - List* returns a slice (empty, never nil) without a hard limit unless
//     stated.
//   - Update*/Decide* are point mutations with explicit optimistic guards.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ErrNotFound is returned when a Get* call finds no row.
var ErrNotFound = errors.New("store: not found")

// ErrConflict reports a fenced write losing its race: a source scan slot
// already claimed, or the requirements merge hitting the key cap.
var ErrConflict = errors.New("store: conflict")

// ErrAlreadyDecided is returned when DecideApproval is called on an approval
// that has already left the PENDING state. Fail closed: never allow a second
// decision to silently overwrite the first.
var ErrAlreadyDecided = types.ErrApprovalAlreadyDecided

// ErrDuplicatePending is returned by CreateApproval when a partial unique index
// (approvals_pending_credential_uniq / approvals_pending_noncred_uniq) rejects a
// second open PENDING approval for the same dedup key — i.e. a concurrent raise
// lost the race. Callers treat it as a dedup signal (re-read the existing PENDING
// row and return it), NOT a hard failure. approval.RequestApproval errors.Is-es
// this exact value; both names alias the one sentinel in internal/types.
var ErrDuplicatePending = types.ErrDuplicatePendingApproval

// ─── AgentRun ────────────────────────────────────────────────────────────────

// CreateRun inserts a new run and returns the persisted row.
func (s PG) CreateRun(ctx context.Context, r types.AgentRun) (types.AgentRun, error) {
	const q = `
		INSERT INTO agent_runs
			(id, created_at, updated_at, created_by, agent, repo, task,
			 policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id, title, description, workspace_ids)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
		RETURNING id, created_at, updated_at, created_by, agent, repo, task,
			policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id, title, description, workspace_ids`

	row := s.Pool.QueryRow(ctx, q,
		r.ID, r.CreatedAt, r.UpdatedAt, r.CreatedBy, r.Agent, r.Repo, r.Task,
		r.PolicyID, string(r.ConfinementClass), string(r.State),
		r.SPIFFEID, r.RunnerTarget, r.SandboxRef, r.Interactive, r.WorkspacePath, r.WorkspaceID, r.SourceID, r.Image, r.AutoStopAfterSec,
		r.AgentExecID, r.Title, r.Description, r.WorkspaceIDs,
	)
	return scanRun(row)
}

// GetRun returns the run for id, or ErrNotFound.
func (s PG) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	const q = `
		SELECT id, created_at, updated_at, created_by, agent, repo, task,
			policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id, title, description, workspace_ids
		FROM agent_runs WHERE id = $1`
	return scanRun(s.Pool.QueryRow(ctx, q, id))
}

// ListRuns returns all runs in reverse creation order (unbounded).
func (s PG) ListRuns(ctx context.Context) ([]types.AgentRun, error) {
	return s.ListRunsPage(ctx, Page{})
}

// UpdateRunStateIf conditionally transitions a run from fromState to toState in
// a single UPDATE ... WHERE id=$ AND state=$from, returning whether the update
// applied. It is the optimistic guard the completion watcher uses: it only
// transitions a run that is STILL in fromState (e.g. RUNNING), so a concurrent
// kill/stop that already moved the run to a terminal state is never clobbered
// (TOCTOU-safe, like DecideApproval). A false return with a nil error means the
// run existed but was no longer in fromState (or did not exist) — the caller
// treats this as "someone else won the transition" and does nothing.
func (s PG) UpdateRunStateIf(ctx context.Context, id uuid.UUID, fromState, toState types.RunState) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now() WHERE id=$2 AND state=$3`,
		string(toState), id, string(fromState),
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// UpdateRunStateIfIdle is UpdateRunStateIf plus an idleness guard: it transitions
// a run from fromState to toState ONLY when the row is still in fromState AND its
// updated_at has NOT advanced past notAfter (the snapshot the caller observed).
// This closes the reaper's idleness TOCTOU: the idle scan reads updated_at in a
// snapshot, but an active `wardyn attach` TouchRun (which bumps updated_at while
// leaving state=RUNNING) can land between snapshot and stop. Guarding only on
// state=RUNNING would then stop the now-active run, defeating the keepalive.
// Passing the snapshot's updated_at as notAfter makes a run touched after the
// snapshot no-op the stop (rows-affected 0 => false), so the reaper leaves it be
// and retries on the next tick. Returns (true, nil) when the transition applied.
func (s PG) UpdateRunStateIfIdle(ctx context.Context, id uuid.UUID, fromState, toState types.RunState, notAfter time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET state=$1, updated_at=now() WHERE id=$2 AND state=$3 AND updated_at <= $4`,
		string(toState), id, string(fromState), notAfter,
	)
	if err != nil {
		return false, fmt.Errorf("store: conditional idle update run state: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetSandboxRef records the runner reference (container ID / pod name).
func (s PG) SetSandboxRef(ctx context.Context, id uuid.UUID, ref string) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET sandbox_ref=$1, updated_at=now() WHERE id=$2`,
		ref, id,
	)
	if err != nil {
		return fmt.Errorf("store: set sandbox ref: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRunImage scoped-writes ONLY the resolved-image provenance column. Called
// once after image resolution (the image is resolved after the row is
// inserted, so this is a scoped update, not a CreateRun column).
func (s PG) SetRunImage(ctx context.Context, id uuid.UUID, image string) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET image=$1, updated_at=now() WHERE id=$2`,
		image, id,
	)
	if err != nil {
		return fmt.Errorf("store: set run image: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// SetRunAgentExecID scoped-writes ONLY the agent_exec_id column. Called once
// right after the driver execs the agent (the exec id exists only after Exec, so
// this is a scoped update, not a CreateRun column value). The crash reconciler
// reads it to observe agent liveness across a restart.
func (s PG) SetRunAgentExecID(ctx context.Context, id uuid.UUID, execID string) error {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE agent_runs SET agent_exec_id=$1, updated_at=now() WHERE id=$2`,
		execID, id,
	)
	if err != nil {
		return fmt.Errorf("store: set run agent exec id: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// TouchRun bumps a run's updated_at to now() without changing any other field.
// It is the activity keepalive the interactive-attach handler calls so the idle
// reaper (which measures idleness by agent_runs.updated_at) does not stop a run
// that a human is actively attached to. Returns ErrNotFound when no row matched.
func (s PG) TouchRun(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `UPDATE agent_runs SET updated_at=now() WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("store: touch run: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// scanRun is the ONE reader for every agent_runs column list in this package
// (CreateRun's RETURNING, GetRun, ListRunsPage, ListRunsPageByCreator,
// ClaimStaleRunWatchers). New columns are APPENDED to the end of all of them
// and to the end of this Scan — appending is the only edit that cannot
// silently transpose two same-typed columns past the compiler.
func scanRun(row pgx.Row) (types.AgentRun, error) {
	var r types.AgentRun
	var cc, state string
	err := row.Scan(
		&r.ID, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.Agent, &r.Repo, &r.Task,
		&r.PolicyID, &cc, &state,
		&r.SPIFFEID, &r.RunnerTarget, &r.SandboxRef, &r.Interactive, &r.WorkspacePath, &r.WorkspaceID, &r.SourceID, &r.Image, &r.AutoStopAfterSec,
		&r.AgentExecID, &r.Title, &r.Description, &r.WorkspaceIDs,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.AgentRun{}, ErrNotFound
	}
	if err != nil {
		return types.AgentRun{}, fmt.Errorf("store: scan run: %w", err)
	}
	r.ConfinementClass = types.ConfinementClass(cc)
	r.State = types.RunState(state)
	return r, nil
}

// ─── RunPolicy ───────────────────────────────────────────────────────────────

// CreatePolicy inserts a policy and returns the persisted row.
func (s PG) CreatePolicy(ctx context.Context, p types.RunPolicy) (types.RunPolicy, error) {
	specJSON, err := json.Marshal(p.Spec)
	if err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: marshal policy spec: %w", err)
	}
	const q = `
		INSERT INTO run_policies (id, name, created_at, updated_at, spec)
		VALUES ($1,$2,$3,$4,$5)
		RETURNING id, name, created_at, updated_at, spec`
	return scanPolicy(s.Pool.QueryRow(ctx, q, p.ID, p.Name, p.CreatedAt, p.UpdatedAt, specJSON))
}

// GetPolicy returns the policy for id, or ErrNotFound.
func (s PG) GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error) {
	const q = `SELECT id, name, created_at, updated_at, spec FROM run_policies WHERE id = $1`
	return scanPolicy(s.Pool.QueryRow(ctx, q, id))
}

// ListPolicies returns all policies in reverse creation order. The slice is
// empty (never nil) when no policies exist.
func (s PG) ListPolicies(ctx context.Context) ([]types.RunPolicy, error) {
	return s.ListPoliciesPage(ctx, Page{})
}

// UpdatePolicy replaces a policy's name and spec and bumps updated_at, returning
// the persisted row. Returns ErrNotFound when no policy has the given id. The
// caller is responsible for validating the spec before calling (policies are
// admin-gated config; the API validates every spec before it reaches the store).
func (s PG) UpdatePolicy(ctx context.Context, id uuid.UUID, name string, spec types.RunPolicySpec) (types.RunPolicy, error) {
	specJSON, err := json.Marshal(spec)
	if err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: marshal policy spec: %w", err)
	}
	const q = `
		UPDATE run_policies SET name=$1, spec=$2, updated_at=now()
		WHERE id=$3
		RETURNING id, name, created_at, updated_at, spec`
	return scanPolicy(s.Pool.QueryRow(ctx, q, name, specJSON, id))
}

// DeletePolicy removes a policy by id. Returns ErrNotFound when no row matched.
// Note: agent_runs.policy_id has NO foreign key, so a delete always succeeds even
// while runs still reference the policy — those runs keep a dangling policy_id.
// The run's authorization envelope survives regardless: dispatch records the
// fully-widened spec as a run.policy.effective event in the append-only audit log.
func (s PG) DeletePolicy(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM run_policies WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("store: delete policy: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

func scanPolicy(row pgx.Row) (types.RunPolicy, error) {
	var p types.RunPolicy
	var specRaw []byte
	err := row.Scan(&p.ID, &p.Name, &p.CreatedAt, &p.UpdatedAt, &specRaw)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunPolicy{}, ErrNotFound
	}
	if err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: scan policy: %w", err)
	}
	if err := json.Unmarshal(specRaw, &p.Spec); err != nil {
		return types.RunPolicy{}, fmt.Errorf("store: unmarshal policy spec: %w", err)
	}
	return p, nil
}

// ─── CredentialGrant ─────────────────────────────────────────────────────────

// CreateGrant inserts a credential grant (eligibility record) and returns it.
func (s PG) CreateGrant(ctx context.Context, g types.CredentialGrant) (types.CredentialGrant, error) {
	specJSON, err := json.Marshal(g.Spec)
	if err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: marshal grant spec: %w", err)
	}
	const q = `
		INSERT INTO credential_grants (id, run_id, created_at, spec)
		VALUES ($1,$2,$3,$4)
		RETURNING id, run_id, created_at, spec`
	return scanGrant(s.Pool.QueryRow(ctx, q, g.ID, g.RunID, g.CreatedAt, specJSON))
}

// ListGrantsByRun returns all grants for a run.
func (s PG) ListGrantsByRun(ctx context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	const q = `SELECT id, run_id, created_at, spec FROM credential_grants WHERE run_id=$1 ORDER BY created_at`
	return collect(ctx, s.Pool, "list", "grants", q, []any{runID}, scanGrant)
}

func scanGrant(row pgx.Row) (types.CredentialGrant, error) {
	var g types.CredentialGrant
	var specRaw []byte
	// No ErrNoRows mapping: the only callers are CreateGrant (INSERT ...
	// RETURNING always yields a row) and the ListGrantsByRun iteration.
	err := row.Scan(&g.ID, &g.RunID, &g.CreatedAt, &specRaw)
	if err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: scan grant: %w", err)
	}
	if err := json.Unmarshal(specRaw, &g.Spec); err != nil {
		return types.CredentialGrant{}, fmt.Errorf("store: unmarshal grant spec: %w", err)
	}
	return g, nil
}

// ─── ApprovalRequest ─────────────────────────────────────────────────────────

// CreateApproval inserts a new approval request.
func (s PG) CreateApproval(ctx context.Context, a types.ApprovalRequest) (types.ApprovalRequest, error) {
	scopeJSON, err := json.Marshal(a.RequestedScope)
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: marshal approval scope: %w", err)
	}
	// The INSERT list deliberately does NOT name decision_scope /
	// decision_expires_at: a newly-raised approval has no decision yet, and
	// both columns' own DEFAULTs ('' / NULL) are exactly "no decision
	// recorded". The RETURNING list DOES name them, so the caller's
	// ApprovalRequest reflects those defaults rather than the Go zero value
	// of a field that was never assigned.
	const q = `
		INSERT INTO approvals
			(id, run_id, grant_id, kind, requested_scope, state, requested_at,
			 decided_at, decided_by, minted_jti, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, run_id, grant_id, kind, requested_scope, state, requested_at,
			decided_at, decided_by, minted_jti, reason, decision_scope, decision_expires_at`
	out, err := scanApproval(s.Pool.QueryRow(ctx, q,
		a.ID, a.RunID, a.GrantID, string(a.Kind), scopeJSON, string(a.State), a.RequestedAt,
		a.DecidedAt, a.DecidedBy, a.MintedJTI, a.Reason,
	))
	if err != nil {
		// A partial unique index (0002 credential / 0022 non-credential) rejecting
		// the insert means a concurrent raise already persisted the open PENDING row.
		// Surface a distinct sentinel so RequestApproval can dedup to the winner
		// instead of erroring.
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return types.ApprovalRequest{}, ErrDuplicatePending
		}
		return types.ApprovalRequest{}, err
	}
	return out, nil
}

// GetApproval returns the approval for id, or ErrNotFound.
func (s PG) GetApproval(ctx context.Context, id uuid.UUID) (types.ApprovalRequest, error) {
	const q = `
		SELECT id, run_id, grant_id, kind, requested_scope, state, requested_at,
			decided_at, decided_by, minted_jti, reason, decision_scope, decision_expires_at
		FROM approvals WHERE id = $1`
	return scanApproval(s.Pool.QueryRow(ctx, q, id))
}

// ListApprovals returns approvals filtered by state. Pass empty string to list all.
func (s PG) ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error) {
	return s.ListApprovalsPage(ctx, stateFilter, Page{})
}

// DecideApproval transitions an approval from PENDING to decision.State.
// Returns ErrAlreadyDecided if the approval is not PENDING (fail-closed).
// Uses a single UPDATE with WHERE state='PENDING' to prevent TOCTOU races.
//
// The SET clause below is a SEPARATE list from the RETURNING clause — update
// only the RETURNING and decision.Scope/ExpiresAt would never persist while
// the RETURNING happily echoes the un-updated row back, a green result over a
// silent no-op. Both lists must carry decision_scope/decision_expires_at.
func (s PG) DecideApproval(ctx context.Context, id uuid.UUID, decision types.ApprovalDecision) (types.ApprovalRequest, error) {
	now := time.Now().UTC()
	const q = `
		UPDATE approvals
		SET state=$1, decided_at=$2, decided_by=$3, reason=$4, decision_scope=$5, decision_expires_at=$6
		WHERE id=$7 AND state='PENDING'
		RETURNING id, run_id, grant_id, kind, requested_scope, state, requested_at,
			decided_at, decided_by, minted_jti, reason, decision_scope, decision_expires_at`
	a, err := scanApproval(s.Pool.QueryRow(ctx, q,
		string(decision.State), now, decision.DecidedBy, decision.Reason,
		string(decision.Scope), decision.ExpiresAt, id,
	))
	if errors.Is(err, ErrNotFound) {
		// Row exists but wasn't PENDING, or doesn't exist at all.
		// Distinguish by checking existence.
		var exists bool
		_ = s.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM approvals WHERE id=$1)`, id).Scan(&exists)
		if exists {
			return types.ApprovalRequest{}, ErrAlreadyDecided
		}
		return types.ApprovalRequest{}, ErrNotFound
	}
	return a, err
}

// scanApproval is the ONE reader for every approvals column list in this
// package (CreateApproval's RETURNING, GetApproval, DecideApproval's
// RETURNING, ListApprovalsPage, ListApprovalsPageByRunCreator). New columns
// are APPENDED to the end of all of them and to the end of this Scan —
// appending is the only edit that cannot silently transpose two same-typed
// columns past the compiler.
func scanApproval(row pgx.Row) (types.ApprovalRequest, error) {
	var a types.ApprovalRequest
	var kind, state, decisionScope string
	var scopeRaw []byte
	err := row.Scan(
		&a.ID, &a.RunID, &a.GrantID, &kind, &scopeRaw, &state, &a.RequestedAt,
		&a.DecidedAt, &a.DecidedBy, &a.MintedJTI, &a.Reason, &decisionScope, &a.DecisionExpiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.ApprovalRequest{}, ErrNotFound
	}
	if err != nil {
		return types.ApprovalRequest{}, fmt.Errorf("store: scan approval: %w", err)
	}
	a.Kind = types.ApprovalKind(kind)
	a.State = types.ApprovalState(state)
	a.RequestedScope = json.RawMessage(scopeRaw)
	a.DecisionScope = types.ApprovalScope(decisionScope)
	return a, nil
}

// ─── AuditEvent ──────────────────────────────────────────────────────────────

// InsertAuditEvent appends a single audit event. Implements audit.Recorder.
// The Postgres trigger blocks UPDATE/DELETE; this function only ever INSERTs.
func InsertAuditEvent(ctx context.Context, pool *pgxpool.Pool, ev types.AuditEvent) error {
	dataJSON, err := json.Marshal(ev.Data)
	if err != nil {
		return fmt.Errorf("store: marshal audit data: %w", err)
	}
	const q = `
		INSERT INTO audit_events
			(id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)`
	if _, err := pool.Exec(ctx, q,
		ev.ID, ev.Time, ev.RunID, string(ev.ActorType), ev.Actor, ev.Action,
		ev.Target, ev.Outcome, ev.SourceIP, dataJSON,
	); err != nil {
		return fmt.Errorf("store: insert audit event: %w", err)
	}
	return nil
}

// QueryAuditEvents returns audit events for a run in time order.
// limit <= 0 means no explicit limit (returns up to 1000).
func (s PG) QueryAuditEvents(ctx context.Context, runID uuid.UUID, limit int) ([]types.AuditEvent, error) {
	if limit <= 0 {
		limit = 1000
	}
	return s.QueryAuditEventsPage(ctx, runID, Page{Limit: limit})
}

// QueryRecentAuditEvents returns the most-recent audit events across ALL runs,
// newest first — the global SIEM-style feed the Audit view renders. Per-run
// queries (QueryAuditEvents) stay chronological; this global tail is reverse-
// chronological and bounded by limit.
func (s PG) QueryRecentAuditEvents(ctx context.Context, limit int) ([]types.AuditEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	return s.QueryRecentAuditEventsPage(ctx, Page{Limit: limit})
}

// LatestAuditEventByAction returns the most recent audit event whose action
// equals the given action, or ErrNotFound when none exists. Used by /healthz to
// find the latest kernel.sensor.heartbeat that drives the eBPF ground-truth
// health state (so the stream reports healthy only while beats are arriving).
func (s PG) LatestAuditEventByAction(ctx context.Context, action string) (types.AuditEvent, error) {
	const q = `
		SELECT id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data
		FROM audit_events WHERE action=$1 ORDER BY seq DESC LIMIT 1`
	ev, err := scanAuditEvent(s.Pool.QueryRow(ctx, q, action))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.AuditEvent{}, ErrNotFound
	}
	if err != nil {
		return types.AuditEvent{}, fmt.Errorf("store: latest audit event by action: %w", err)
	}
	return ev, nil
}

func scanAuditEvent(row pgx.Row) (types.AuditEvent, error) {
	var ev types.AuditEvent
	var actorType string
	var dataRaw []byte
	if err := row.Scan(
		&ev.ID, &ev.Time, &ev.RunID, &actorType, &ev.Actor, &ev.Action,
		&ev.Target, &ev.Outcome, &ev.SourceIP, &dataRaw,
	); err != nil {
		return types.AuditEvent{}, err
	}
	ev.ActorType = types.ActorType(actorType)
	if len(dataRaw) > 0 {
		ev.Data = json.RawMessage(dataRaw)
	}
	return ev, nil
}

// ─── SiteConfig ──────────────────────────────────────────────────────────────

// GetSiteConfig returns the operator-wide site config, or a ZERO-VALUE
// SiteConfig (not an error) when no row has been written yet — first boot has
// no config, and "unconfigured" is a valid, common state rather than a
// failure the caller must special-case.
func (s PG) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, `SELECT config FROM site_config WHERE singleton`).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.SiteConfig{}, nil
	}
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: get site config: %w", err)
	}
	var cfg types.SiteConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: unmarshal site config: %w", err)
	}
	return cfg, nil
}

// PutSiteConfig upserts the single operator-wide site config row and returns
// the persisted value. The `singleton` primary key (CHECKed true) makes a
// second row impossible at the schema level; a write always REPLACES the whole
// document (no partial merge — the API layer decodes and validates the full
// document before calling this).
func (s PG) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	raw, err := json.Marshal(cfg)
	if err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: marshal site config: %w", err)
	}
	const q = `
		INSERT INTO site_config (singleton, config, updated_at)
		VALUES (true, $1, now())
		ON CONFLICT (singleton) DO UPDATE SET config = EXCLUDED.config, updated_at = now()
		RETURNING config`
	var out []byte
	if err := s.Pool.QueryRow(ctx, q, raw).Scan(&out); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: put site config: %w", err)
	}
	var saved types.SiteConfig
	if err := json.Unmarshal(out, &saved); err != nil {
		return types.SiteConfig{}, fmt.Errorf("store: unmarshal saved site config: %w", err)
	}
	return saved, nil
}
