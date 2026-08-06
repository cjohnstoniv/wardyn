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
			 policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		RETURNING id, created_at, updated_at, created_by, agent, repo, task,
			policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id`

	row := s.Pool.QueryRow(ctx, q,
		r.ID, r.CreatedAt, r.UpdatedAt, r.CreatedBy, r.Agent, r.Repo, r.Task,
		r.PolicyID, string(r.ConfinementClass), string(r.State),
		r.SPIFFEID, r.RunnerTarget, r.SandboxRef, r.Interactive, r.WorkspacePath, r.WorkspaceID, r.SourceID, r.Image, r.AutoStopAfterSec,
		r.AgentExecID,
	)
	return scanRun(row)
}

// GetRun returns the run for id, or ErrNotFound.
func (s PG) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	const q = `
		SELECT id, created_at, updated_at, created_by, agent, repo, task,
			policy_id, confinement_class, state, spiffe_id, runner_target, sandbox_ref, interactive, workspace_path, workspace_id, source_id, image, auto_stop_after_sec, agent_exec_id
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

func scanRun(row pgx.Row) (types.AgentRun, error) {
	var r types.AgentRun
	var cc, state string
	err := row.Scan(
		&r.ID, &r.CreatedAt, &r.UpdatedAt, &r.CreatedBy, &r.Agent, &r.Repo, &r.Task,
		&r.PolicyID, &cc, &state,
		&r.SPIFFEID, &r.RunnerTarget, &r.SandboxRef, &r.Interactive, &r.WorkspacePath, &r.WorkspaceID, &r.SourceID, &r.Image, &r.AutoStopAfterSec,
		&r.AgentExecID,
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

// ─── Workspace ───────────────────────────────────────────────────────────────

// workspaceProfileParam converts a (possibly empty) json.RawMessage into the
// value pgx should bind for the nullable `profile` JSONB column: an empty
// RawMessage inserts SQL NULL (not yet scanned) rather than the literal JSON
// "null", so GetWorkspace/ListWorkspaces round-trip a pending_scan row with a
// nil Profile, not a 4-byte "null" blob.
func workspaceProfileParam(p json.RawMessage) any {
	if len(p) == 0 {
		return nil
	}
	return []byte(p)
}

// workspaceApprovedParam serializes the operator-owned approved-egress list
// for its JSONB column; empty ⇒ NULL.
func workspaceApprovedParam(domains []string) any {
	if len(domains) == 0 {
		return nil
	}
	b, err := json.Marshal(domains)
	if err != nil {
		return nil // unreachable for []string; fail-safe to "nothing approved"
	}
	return b
}

// workspaceLLMCredParam serializes the operator-owned model/harness cred binding
// for its JSONB column; nil ⇒ NULL (no binding).
func workspaceLLMCredParam(c *types.WorkspaceLLMCred) any {
	if c == nil {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil // fail-safe to "no binding"
	}
	return b
}

// workspaceSourcesParam serializes a workspace's source composition for its
// NOT NULL sources JSONB column. Unlike the other JSONB helpers here, this one
// NEVER returns nil: a nil/empty slice still marshals to the JSON array "[]"
// (never SQL NULL), matching the column's NOT NULL constraint (0029).
func workspaceSourcesParam(sources []types.WorkspaceSource) any {
	if sources == nil {
		sources = []types.WorkspaceSource{}
	}
	b, err := json.Marshal(sources)
	if err != nil {
		return []byte("[]") // unreachable for this concrete type; fail-safe
	}
	return b
}

// workspaceBaseImageParam serializes a workspace's base-image choice for its
// nullable base_image JSONB column; nil ⇒ NULL (no explicit choice recorded).
func workspaceBaseImageParam(img *types.WorkspaceBaseImage) any {
	if img == nil {
		return nil
	}
	b, err := json.Marshal(img)
	if err != nil {
		return nil // fail-safe to "no base image recorded"
	}
	return b
}

// workspaceRequirementsParam serializes a workspace's requirements contract
// for its nullable requirements JSONB column; empty/nil ⇒ NULL, mirroring
// workspaceApprovedParam.
func workspaceRequirementsParam(reqs map[string]types.WorkspaceRequirement) any {
	if len(reqs) == 0 {
		return nil
	}
	b, err := json.Marshal(reqs)
	if err != nil {
		return nil // unreachable for this concrete type; fail-safe to "none declared"
	}
	return b
}

// workspaceAttachmentsParam marshals a workspace's attachments for storage.
// Empty writes '[]' (the column is NOT NULL — 0031 backfills every row): an
// empty array IS the pre-split marker, and the hydrate pass falls back to the
// embedded sources column exactly as it would for NULL.
func workspaceAttachmentsParam(atts []types.WorkspaceAttachment) []byte {
	if len(atts) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(atts)
	if err != nil {
		return []byte("[]") // unreachable for this concrete type; fail-safe to the pre-split marker — NOT nil, the column is NOT NULL (matches workspaceSourcesParam)
	}
	return b
}

// wsCols is the canonical workspace column list (order matches scanWorkspace).
const wsCols = `id, name, sources, base_image, requirements, profile, image_ref, ` +
	`built_profile_hash, approved_egress, active_run_id, status, created_at, updated_at, ` +
	`record_results, llm_cred, attachments, base_image_id`

// CreateWorkspace inserts an onboarded workspace and returns the persisted
// row. Profile is internal/workspacescan's opaque WorkspaceProfile blob (nil
// until scanned).
func (s PG) CreateWorkspace(ctx context.Context, ws types.Workspace) (types.Workspace, error) {
	q := `
		INSERT INTO workspaces (` + wsCols + `)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
		RETURNING ` + wsCols
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx, q,
		ws.ID, ws.Name, workspaceSourcesParam(ws.Sources), workspaceBaseImageParam(ws.BaseImage),
		workspaceRequirementsParam(ws.Requirements), workspaceProfileParam(ws.Profile), ws.ImageRef,
		ws.BuiltProfileHash, workspaceApprovedParam(ws.ApprovedEgress), ws.ActiveRunID,
		string(ws.Status), ws.CreatedAt, ws.UpdatedAt, workspaceProfileParam(ws.RecordResults),
		workspaceLLMCredParam(ws.LLMCred), workspaceAttachmentsParam(ws.Attachments), ws.BaseImageID,
	))
}

// GetWorkspace returns the workspace for id, or ErrNotFound.
func (s PG) GetWorkspace(ctx context.Context, id uuid.UUID) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx, `SELECT `+wsCols+` FROM workspaces WHERE id = $1`, id))
}

// ListWorkspaces returns all workspaces in reverse creation order. The slice
// is empty (never nil) when no workspaces exist.
func (s PG) ListWorkspaces(ctx context.Context) ([]types.Workspace, error) {
	return s.ListWorkspacesPage(ctx, Page{})
}

// UpdateWorkspace replaces a workspace's editable composition (name, sources,
// base_image, requirements) and bumps updated_at, returning the persisted row.
// It is a FULL-column write (it also sets profile, image_ref,
// built_profile_hash, status and the other scan-owned columns), which is why
// callers must round-trip the fetched row. Returns ErrNotFound when no
// workspace has the given id.
//
// handleUpdateWorkspace does round-trip, and resets the scan-owned fields +
// ApprovedEgress itself when the composition changed — the persisted profile
// and egress approvals were reviewed against the OLD sources.
func (s PG) UpdateWorkspace(ctx context.Context, id uuid.UUID, ws types.Workspace) (types.Workspace, error) {
	q := `
		UPDATE workspaces
		SET name=$1, sources=$2, base_image=$3, requirements=$4,
			profile=$5, image_ref=$6, built_profile_hash=$7, approved_egress=$8,
			active_run_id=$9, status=$10, record_results=$11, llm_cred=$12,
			attachments=$13, base_image_id=$14, updated_at=now()
		WHERE id=$15
		RETURNING ` + wsCols
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx, q,
		ws.Name, workspaceSourcesParam(ws.Sources), workspaceBaseImageParam(ws.BaseImage),
		workspaceRequirementsParam(ws.Requirements), workspaceProfileParam(ws.Profile), ws.ImageRef,
		ws.BuiltProfileHash, workspaceApprovedParam(ws.ApprovedEgress), ws.ActiveRunID,
		string(ws.Status), workspaceProfileParam(ws.RecordResults), workspaceLLMCredParam(ws.LLMCred),
		workspaceAttachmentsParam(ws.Attachments), ws.BaseImageID, id,
	))
}

// SetWorkspaceLLMCred replaces ONLY the operator-owned model/harness cred
// binding column (plus updated_at), returning the updated row. Scoped like
// SetWorkspaceApprovedEgress so it can never clobber a concurrently-persisted
// async scan. Pass nil to clear the binding.
func (s PG) SetWorkspaceLLMCred(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET llm_cred=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceLLMCredParam(cred), id))
}

// SetWorkspaceApprovedEgress replaces ONLY the operator-owned approved-egress
// column (plus updated_at), returning the updated row. Scoped on purpose: an
// approval must never clobber a concurrently-persisted scan (an async repo
// scan's profile/status land via the full-column UpdateWorkspace, and a
// read-modify-write here would silently revert them).
func (s PG) SetWorkspaceApprovedEgress(ctx context.Context, id uuid.UUID, domains []string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET approved_egress=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceApprovedParam(domains), id))
}

// SetWorkspaceRequirements replaces ONLY the requirements-contract column
// (plus updated_at), returning the updated row. Scoped write, same
// anti-clobber discipline as SetWorkspaceApprovedEgress: it can never clobber
// a concurrently-persisted async scan (profile/status land via the full-column
// UpdateWorkspace, and a read-modify-write here would silently revert them).
// Pass the FULL desired map — like SetWorkspaceApprovedEgress, this replaces
// rather than merges; a caller adding one requirement to an existing set reads
// first, merges in Go, then calls this with the result.
func (s PG) SetWorkspaceRequirements(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET requirements=$1, updated_at=now() WHERE id=$2 RETURNING `+wsCols,
		workspaceRequirementsParam(reqs), id))
}

// SetWorkspaceRecordResult atomically upserts ONE task's entry in the Record
// Mode record_results map (jsonb || merge — never a whole-map read-modify-
// write, so concurrent writers of DIFFERENT tasks can never lose each other's
// entries). When onlyIfStatus is non-empty the write applies only while the
// task's CURRENT stored status equals it (single-statement compare-and-set):
// a late streaming upload can never revert a completed capture, and a double
// capture no-ops. Returns applied=false (no error) on a guard miss.
func (s PG) SetWorkspaceRecordResult(ctx context.Context, id uuid.UUID,
	taskKey string, result json.RawMessage, onlyIfStatus string) (types.Workspace, bool, error) {
	q := `UPDATE workspaces
		SET record_results = COALESCE(record_results,'{}'::jsonb) || jsonb_build_object($2::text, $3::jsonb),
			updated_at=now()
		WHERE id=$1`
	args := []any{id, taskKey, string(result)}
	if onlyIfStatus != "" {
		q += ` AND COALESCE(record_results->$2->>'status','') = $4`
		args = append(args, onlyIfStatus)
	}
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx, q+` RETURNING `+wsCols, args...))
	ws, err = s.hydrateIfOK(ctx, ws, err)
	if errors.Is(err, ErrNotFound) && onlyIfStatus != "" {
		// Distinguish guard-miss from a missing workspace.
		ws, gerr := s.GetWorkspace(ctx, id)
		if gerr != nil {
			return types.Workspace{}, false, gerr
		}
		return ws, false, nil
	}
	if err != nil {
		return types.Workspace{}, false, err
	}
	return ws, true, nil
}

// ClaimWorkspaceActiveRun compare-and-sets active_run_id from expected
// (possibly nil) to runID — the atomic serial-import-step gate. Two concurrent
// step launches that both observed the same free slot cannot both win: the
// loser gets applied=false and must NOT launch. Returns ErrNotFound only when
// the workspace does not exist.
func (s PG) ClaimWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID, expected *uuid.UUID) (types.Workspace, bool, error) {
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET active_run_id=$2, updated_at=now()
		 WHERE id=$1 AND active_run_id IS NOT DISTINCT FROM $3 RETURNING `+wsCols,
		id, runID, expected))
	ws, err = s.hydrateIfOK(ctx, ws, err)
	if errors.Is(err, ErrNotFound) {
		ws, gerr := s.GetWorkspace(ctx, id)
		if gerr != nil {
			return types.Workspace{}, false, gerr
		}
		return ws, false, nil
	}
	if err != nil {
		return types.Workspace{}, false, err
	}
	return ws, true, nil
}

// ClearWorkspaceActiveRun clears active_run_id ONLY while it still points at
// runID (conditional, single statement) — a terminal run's cleanup can never
// clobber a step that was concurrently launched and now owns the pointer.
func (s PG) ClearWorkspaceActiveRun(ctx context.Context, id, runID uuid.UUID) (bool, error) {
	tag, err := s.Pool.Exec(ctx,
		`UPDATE workspaces SET active_run_id=NULL, updated_at=now() WHERE id=$1 AND active_run_id=$2`,
		id, runID)
	if err != nil {
		return false, fmt.Errorf("store: clear active run: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetWorkspaceBuiltImage scoped-writes ONLY the image cache columns (the
// build-once/reuse-many cache) — the anti-clobber discipline the other scoped
// writers established; the previous full-row cache write could revert every
// concurrently-persisted async field from a stale snapshot.
func (s PG) SetWorkspaceBuiltImage(ctx context.Context, id uuid.UUID, imageRef, builtHash string) (types.Workspace, error) {
	return s.hydratedScan(ctx, s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET image_ref=$1, built_profile_hash=$2, updated_at=now() WHERE id=$3 RETURNING `+wsCols,
		imageRef, builtHash, id))
}

// SetWorkspaceImportState is the scoped writer the scan orchestrator uses to
// advance status + the in-flight run pointer without a full-row read-modify-
// write.
//
// FENCED (same shape as ClaimSourceActiveRun / SetSourceScanResult): the write is conditional on the
// import-step slot still holding expectedActive, so a caller that decided what
// to write from a STALE read cannot land it. Every caller here does check-then-
// act (read the workspace, decide, write), and this was the only unfenced
// workspace writer — a finalize/update racing a live scan/record run could
// overwrite the fresher state the concurrent run had just written, which is
// exactly the class of race the C001 finalize guard closed at one call site
// only. Pass expectedActive = the active_run_id observed in the read the
// decision came from (nil means "expected no in-flight run"); applied=false
// means the slot moved under the caller, which must then re-read rather than
// retry blindly. Returns ErrNotFound only when the workspace does not exist.
func (s PG) SetWorkspaceImportState(ctx context.Context, id uuid.UUID,
	status types.WorkspaceStatus, activeRunID *uuid.UUID, expectedActive *uuid.UUID) (types.Workspace, bool, error) {
	// IS NOT DISTINCT FROM (not `=`) so a nil expectedActive correctly matches a
	// NULL slot — `active_run_id = NULL` is never true in SQL.
	ws, err := scanWorkspace(s.Pool.QueryRow(ctx,
		`UPDATE workspaces SET status=$1, active_run_id=$2, updated_at=now()
		 WHERE id=$3 AND active_run_id IS NOT DISTINCT FROM $4 RETURNING `+wsCols,
		string(status), activeRunID, id, expectedActive))
	ws, err = s.hydrateIfOK(ctx, ws, err)
	if errors.Is(err, ErrNotFound) {
		// Distinguish a guard miss (slot moved) from a missing workspace.
		cur, gerr := s.GetWorkspace(ctx, id)
		if gerr != nil {
			return types.Workspace{}, false, gerr
		}
		return cur, false, nil
	}
	if err != nil {
		return types.Workspace{}, false, err
	}
	return ws, true, nil
}

// DeleteWorkspace removes a workspace by id. Returns ErrNotFound when no row matched.
func (s PG) DeleteWorkspace(ctx context.Context, id uuid.UUID) error {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM workspaces WHERE id=$1`, id)
	if err != nil {
		return fmt.Errorf("store: delete workspace: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// hydratedScan wraps scanWorkspace for the single-row writers: decode the row,
// then materialize the derived view (attachments → Sources/Profile/Status/
// BaseImage + the folded EffectiveRequirements).

// hydrateIfOK hydrates ws when err is nil — the mixed-return writers' shim.
func (s PG) hydrateIfOK(ctx context.Context, ws types.Workspace, err error) (types.Workspace, error) {
	if err != nil {
		return ws, err
	}
	return s.hydrated(ctx, ws)
}

func (s PG) hydratedScan(ctx context.Context, row pgx.Row) (types.Workspace, error) {
	ws, err := scanWorkspace(row)
	if err != nil {
		return ws, err
	}
	return s.hydrated(ctx, ws)
}

func scanWorkspace(row pgx.Row) (types.Workspace, error) {
	var ws types.Workspace
	var status string
	var sourcesRaw, baseImageRaw, requirementsRaw, profileRaw, approvedRaw, recordRaw, llmCredRaw, attachmentsRaw []byte
	err := row.Scan(
		&ws.ID, &ws.Name, &sourcesRaw, &baseImageRaw, &requirementsRaw,
		&profileRaw, &ws.ImageRef, &ws.BuiltProfileHash, &approvedRaw, &ws.ActiveRunID,
		&status, &ws.CreatedAt, &ws.UpdatedAt, &recordRaw, &llmCredRaw,
		&attachmentsRaw, &ws.BaseImageID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.Workspace{}, ErrNotFound
	}
	if err != nil {
		return types.Workspace{}, fmt.Errorf("store: scan workspace: %w", err)
	}
	ws.Status = types.WorkspaceStatus(status)
	if profileRaw != nil {
		ws.Profile = json.RawMessage(profileRaw)
	}
	if recordRaw != nil {
		ws.RecordResults = json.RawMessage(recordRaw)
	}
	// sources is NOT NULL; malformed JSONB is unreachable via
	// workspaceSourcesParam, but fail safe to "no sources" rather than error.
	_ = json.Unmarshal(sourcesRaw, &ws.Sources)
	if baseImageRaw != nil {
		var bi types.WorkspaceBaseImage
		if json.Unmarshal(baseImageRaw, &bi) == nil {
			ws.BaseImage = &bi
		}
	}
	if requirementsRaw != nil {
		// Malformed JSONB is unreachable via workspaceRequirementsParam; on the
		// off chance, fail safe to "no requirements declared" rather than error.
		_ = json.Unmarshal(requirementsRaw, &ws.Requirements)
	}
	if approvedRaw != nil {
		// Malformed JSONB is unreachable via workspaceApprovedParam; on the
		// off chance, fail safe to "nothing approved" rather than error.
		_ = json.Unmarshal(approvedRaw, &ws.ApprovedEgress)
	}
	if llmCredRaw != nil {
		var c types.WorkspaceLLMCred
		if json.Unmarshal(llmCredRaw, &c) == nil {
			ws.LLMCred = &c
		}
	}
	if attachmentsRaw != nil {
		_ = json.Unmarshal(attachmentsRaw, &ws.Attachments) // fail safe: pre-split view
	}
	return ws, nil
}

// deriveWorkspaceMirrors populates ws.Kind/Source/Ref/DefaultTarget as
// READ-ONLY convenience mirrors of ws.Sources[0], for single-source workspaces
// only — the shape pkg/client and `wardyn workspace list` still render. A
// multi-source workspace has no single "the" kind/source/ref/target, so the
// mirrors are left zero rather than arbitrarily picking one. Derived purely
// from Sources[0] (never BaseImage): a migrated legacy 'container' workspace
// (now an ephemeral source + a custom base image) mirrors as
// Kind="ephemeral", not Kind="container" — the composition model has no
// single-field notion of "container" to reconstruct. These fields are NEVER
// persisted; they exist on the Go struct only after this derivation.
func deriveWorkspaceMirrors(ws *types.Workspace) {
	if len(ws.Sources) != 1 {
		return
	}
	src := ws.Sources[0]
	ws.Kind = types.WorkspaceKind(src.Type)
	ws.Ref = src.Ref
	ws.DefaultTarget = src.Target
	switch src.Type {
	case types.WorkspaceSourceTypeLocalDir:
		ws.Source = src.Path
	case types.WorkspaceSourceTypeRepo:
		ws.Source = src.Source
		// ephemeral carries neither Path nor Source; ws.Source stays "".
	}
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
	const q = `
		INSERT INTO approvals
			(id, run_id, grant_id, kind, requested_scope, state, requested_at,
			 decided_at, decided_by, minted_jti, reason)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, run_id, grant_id, kind, requested_scope, state, requested_at,
			decided_at, decided_by, minted_jti, reason`
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
			decided_at, decided_by, minted_jti, reason
		FROM approvals WHERE id = $1`
	return scanApproval(s.Pool.QueryRow(ctx, q, id))
}

// ListApprovals returns approvals filtered by state. Pass empty string to list all.
func (s PG) ListApprovals(ctx context.Context, stateFilter types.ApprovalState) ([]types.ApprovalRequest, error) {
	return s.ListApprovalsPage(ctx, stateFilter, Page{})
}

// DecideApproval transitions an approval from PENDING to the given state.
// Returns ErrAlreadyDecided if the approval is not PENDING (fail-closed).
// Uses a single UPDATE with WHERE state='PENDING' to prevent TOCTOU races.
func (s PG) DecideApproval(ctx context.Context, id uuid.UUID, state types.ApprovalState, decidedBy, reason string) (types.ApprovalRequest, error) {
	now := time.Now().UTC()
	const q = `
		UPDATE approvals
		SET state=$1, decided_at=$2, decided_by=$3, reason=$4
		WHERE id=$5 AND state='PENDING'
		RETURNING id, run_id, grant_id, kind, requested_scope, state, requested_at,
			decided_at, decided_by, minted_jti, reason`
	a, err := scanApproval(s.Pool.QueryRow(ctx, q, string(state), now, decidedBy, reason, id))
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

func scanApproval(row pgx.Row) (types.ApprovalRequest, error) {
	var a types.ApprovalRequest
	var kind, state string
	var scopeRaw []byte
	err := row.Scan(
		&a.ID, &a.RunID, &a.GrantID, &kind, &scopeRaw, &state, &a.RequestedAt,
		&a.DecidedAt, &a.DecidedBy, &a.MintedJTI, &a.Reason,
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
