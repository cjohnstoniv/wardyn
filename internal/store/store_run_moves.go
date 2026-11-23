// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Moves of a paused run to another machine (run_moves) and a moved run's placement history (run_placements).
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunMoveOrphanAfter is how long a move holds its run after its executor's last heartbeat. A row whose heartbeat
// (or, never stamped, its admission) is older holds nothing: RunMoveInFlight answers false for it, so a Resume is
// admitted, and the move sweep fails or releases it.
const RunMoveOrphanAfter = 5 * time.Minute

// RunMoveHoldGuard is how long before RunMoveOrphanAfter a heartbeat stops renewing a hold. An executor whose last
// stamp is that close to the bound cannot renew a hold a Resume is about to read as lapsed.
const RunMoveHoldGuard = time.Minute

// runMoveAdmitLockClass serializes move admissions, so the in-flight caps are counted under one lock.
const runMoveAdmitLockClass int32 = 0x574D4F56 // ASCII "WMOV"

// ErrRunMoveBusy: a move already holds the run, or a cap on moves in flight is reached.
var ErrRunMoveBusy = errors.New("store: a move holds the run, or too many moves are in flight")

// The commit's refusals. Each leaves every row as it was.
var (
	// ErrRunMoveNotImporting: the move row is no longer importing (the sweep failed it, or it never got there).
	ErrRunMoveNotImporting = errors.New("store: the move is no longer importing")
	// ErrRunMoveRunEnded: the run left RUNNING, or was lost.
	ErrRunMoveRunEnded = errors.New("store: the run ended or was lost during the move")
	// ErrRunMovePauseChanged: the run is no longer in the stopped pause the move made, at the move's generation.
	ErrRunMovePauseChanged = errors.New("store: the run's pause changed during the move")
	// ErrRunMoveSourceUnsettled: the source still has an unsettled action, an exit with an incomplete tail, a
	// containment-pending row or an open teardown hold for the run.
	ErrRunMoveSourceUnsettled = errors.New("store: the source runner has not settled the run")
)

// runMoveHeldSQL is the hold: a row in flight, or committed and not released, whose executor stamped it (or, never
// stamped, admitted it) within $2. $1 is the run, or NULL for every run.
const runMoveHeldSQL = `(state IN ('admitted','exporting','importing') OR (state = 'committed' AND released_at IS NULL))
	AND COALESCE(heartbeat_at, created_at) >= now() - $2::interval`

// RunMoveLimits caps the moves in flight at admission. Zero means no cap.
type RunMoveLimits struct {
	// Install caps the moves in flight on the whole install.
	Install int
	// PerRunner caps the moves in flight onto the move's destination runner; it applies only when the move names one.
	PerRunner int
}

// RunMoveCommit is what a move's commit writes on the run row beyond the row's own destination columns: the facts
// the move does not record, the kept grants' deliveries at the destination, and what was carried.
type RunMoveCommit struct {
	MoveID, RunID   uuid.UUID
	PlacementFilled bool
	PoolRevision    int64
	PoolSelection   types.RunnerPoolSelection
	// Deliveries is each kept or dropped grant's delivery mode at the destination, by grant id ("" for a remote
	// destination).
	Deliveries     map[uuid.UUID]string
	ManifestSHA256 string
	Bytes          int64
	Skipped        int
}

// RunMoveDue is a row the move sweep has work on, and whether its holder's heartbeat is past RunMoveOrphanAfter.
type RunMoveDue struct {
	Move  types.RunMove
	Stale bool
}

// RunMoveStore records moves of a run. Optional for RunLeaser's reason: the api layer type-asserts, and production
// is always PG.
type RunMoveStore interface {
	// AdmitRunMove writes mv as admitted, counted against limits under the admission lock; ErrRunMoveBusy when a
	// move holds the run or a cap is reached. It fills mv's id when unset and returns the stored row.
	AdmitRunMove(ctx context.Context, mv types.RunMove, limits RunMoveLimits) (types.RunMove, error)
	// RunMoveInFlight reports whether a move holds the run: one in flight, or committed and not released, whose
	// heartbeat is within RunMoveOrphanAfter.
	RunMoveInFlight(ctx context.Context, runID uuid.UUID) (bool, error)
	// GetRunMove reads one move; ErrNotFound when there is none.
	GetRunMove(ctx context.Context, id uuid.UUID) (types.RunMove, error)
	// ListRunMoves is every move of the run, oldest first, refused ones included.
	ListRunMoves(ctx context.Context, runID uuid.UUID) ([]types.RunMove, error)
	// ListRunPlacements is the run's placement history by seq; empty for a run that never moved.
	ListRunPlacements(ctx context.Context, runID uuid.UUID) ([]types.RunPlacement, error)
	// SetRunMovePauseGen records the stopped row's pause generation on a move in flight.
	SetRunMovePauseGen(ctx context.Context, id uuid.UUID, gen int64) (bool, error)
	// SetRunMoveDestination records what the move knows of its destination on a move in flight: mv's To fields,
	// ToTokenJTI and Narrowed.
	SetRunMoveDestination(ctx context.Context, mv types.RunMove) (bool, error)
	// AdvanceRunMove moves a row from one in-flight state to the next; false when it is not in from.
	AdvanceRunMove(ctx context.Context, id uuid.UUID, from, to types.MoveState) (bool, error)
	// StampRunMoveHeartbeat renews the executor's hold. False when the hold has lapsed or is within
	// RunMoveHoldGuard of lapsing, or the row is released or ended: the executor then stops.
	StampRunMoveHeartbeat(ctx context.Context, id uuid.UUID) (bool, error)
	// EndRunMove ends a move in flight as refused or failed with its reason; cleanup is pending when a destination
	// exists to tear down. False when the row is no longer in flight.
	EndRunMove(ctx context.Context, id uuid.UUID, state types.MoveState, failure string, cleanup types.MoveCleanup) (bool, error)
	// CommitRunMove binds the run to the move's destination in one transaction (the move must be importing).
	CommitRunMove(ctx context.Context, c RunMoveCommit) error
	// ReleaseRunMove ends a committed move's hold. With onlyStale it releases only a hold whose heartbeat is past
	// RunMoveOrphanAfter, as the sweep does.
	ReleaseRunMove(ctx context.Context, id uuid.UUID, onlyStale bool) (bool, error)
	// FailStaleRunMove fails a move in flight whose heartbeat is past RunMoveOrphanAfter, with failure and cleanup
	// pending.
	FailStaleRunMove(ctx context.Context, id uuid.UUID, failure string) (bool, error)
	// SetRunMoveCleanup and SetRunMoveSourceRevoke record a pending step as done.
	SetRunMoveCleanup(ctx context.Context, id uuid.UUID, c types.MoveCleanup) error
	SetRunMoveSourceRevoke(ctx context.Context, id uuid.UUID, r types.MoveSourceRevoke) error
	// ListRunMovesDue is every row with sweep work: a held row (stale or not), a pending cleanup or a pending
	// source revoke. At most limit rows, oldest first.
	ListRunMovesDue(ctx context.Context, limit int) ([]RunMoveDue, error)
	// ListRunMovesFromRunner is every committed move off runnerID whose cleanup is still pending.
	ListRunMovesFromRunner(ctx context.Context, runnerID uuid.UUID) ([]types.RunMove, error)
}

var _ RunMoveStore = PG{}

const runMoveCols = `id, run_id, state, pause_gen,
	from_placement, from_runner_id, from_pool_id, from_executor, from_sandbox_ref, from_evidence_source, from_trust_class,
	to_placement, to_runner_id, to_pool_id, to_executor, to_sandbox_ref, to_evidence_source, to_trust_class, to_token_jti,
	cleanup, source_revoke, recording_level, COALESCE(moved_by,''), COALESCE(manifest_sha256,''), COALESCE(bytes,0),
	COALESCE(skipped,0), narrowed, COALESCE(failure,''), heartbeat_at, released_at, created_at, updated_at, committed_at`

func scanRunMove(row pgx.Row) (types.RunMove, error) {
	var mv types.RunMove
	err := row.Scan(&mv.ID, &mv.RunID, &mv.State, &mv.PauseGen,
		&mv.FromPlacement, &mv.FromRunnerID, &mv.FromPoolID, &mv.FromExecutor, &mv.FromSandboxRef, &mv.FromEvidenceSource, &mv.FromTrustClass,
		&mv.ToPlacement, &mv.ToRunnerID, &mv.ToPoolID, &mv.ToExecutor, &mv.ToSandboxRef, &mv.ToEvidenceSource, &mv.ToTrustClass, &mv.ToTokenJTI,
		&mv.Cleanup, &mv.SourceRevoke, &mv.RecordingLevel, &mv.MovedBy, &mv.ManifestSHA256, &mv.Bytes,
		&mv.Skipped, &mv.Narrowed, &mv.Failure, &mv.HeartbeatAt, &mv.ReleasedAt, &mv.CreatedAt, &mv.UpdatedAt, &mv.CommittedAt)
	return mv, err
}

// AdmitRunMove — see RunMoveStore.
func (s PG) AdmitRunMove(ctx context.Context, mv types.RunMove, limits RunMoveLimits) (types.RunMove, error) {
	if mv.ID == uuid.Nil {
		mv.ID = uuid.New()
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.RunMove{}, fmt.Errorf("store: admit run move: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transaction is closed
	if err := lockScope(ctx, tx, runMoveAdmitLockClass, "admit"); err != nil {
		return types.RunMove{}, err
	}
	var held bool
	var install, onto int
	if err := tx.QueryRow(ctx, `SELECT
		EXISTS (SELECT 1 FROM run_moves WHERE run_id = $1 AND `+runMoveHeldSQL+`),
		(SELECT count(*) FROM run_moves WHERE state IN ('admitted','exporting','importing') AND COALESCE(heartbeat_at, created_at) >= now() - $2::interval),
		(SELECT count(*) FROM run_moves WHERE $3::uuid IS NOT NULL AND to_runner_id = $3 AND state IN ('admitted','exporting','importing')
			AND COALESCE(heartbeat_at, created_at) >= now() - $2::interval)`,
		mv.RunID, RunMoveOrphanAfter.String(), mv.ToRunnerID).Scan(&held, &install, &onto); err != nil {
		return types.RunMove{}, fmt.Errorf("store: admit run move: %w", err)
	}
	if held || (limits.Install > 0 && install >= limits.Install) || (limits.PerRunner > 0 && mv.ToRunnerID != nil && onto >= limits.PerRunner) {
		return types.RunMove{}, ErrRunMoveBusy
	}
	stored, err := scanRunMove(tx.QueryRow(ctx, `INSERT INTO run_moves (id, run_id, state, pause_gen,
		from_placement, from_runner_id, from_pool_id, from_executor, from_sandbox_ref, from_evidence_source, from_trust_class,
		to_placement, to_runner_id, to_pool_id, to_executor, to_evidence_source, to_trust_class, recording_level, moved_by)
		VALUES ($1, $2, 'admitted', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, NULLIF($18, ''))
		RETURNING `+runMoveCols,
		mv.ID, mv.RunID, mv.PauseGen, mv.FromPlacement, mv.FromRunnerID, mv.FromPoolID, mv.FromExecutor, mv.FromSandboxRef,
		mv.FromEvidenceSource, mv.FromTrustClass, mv.ToPlacement, mv.ToRunnerID, mv.ToPoolID, mv.ToExecutor,
		mv.ToEvidenceSource, mv.ToTrustClass, mv.RecordingLevel, mv.MovedBy))
	if pgErr := (*pgconn.PgError)(nil); errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return types.RunMove{}, ErrRunMoveBusy
	}
	if err != nil {
		return types.RunMove{}, fmt.Errorf("store: admit run move: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.RunMove{}, fmt.Errorf("store: admit run move: %w", err)
	}
	return stored, nil
}

// RunMoveInFlight — see RunMoveStore. The statement is the hold every reader shares: admission's move_busy and a
// Resume's refusal.
func (s PG) RunMoveInFlight(ctx context.Context, runID uuid.UUID) (bool, error) {
	var held bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM run_moves WHERE run_id = $1 AND `+runMoveHeldSQL+`)`,
		runID, RunMoveOrphanAfter.String()).Scan(&held); err != nil {
		return false, fmt.Errorf("store: run move in flight: %w", err)
	}
	return held, nil
}

// GetRunMove — see RunMoveStore.
func (s PG) GetRunMove(ctx context.Context, id uuid.UUID) (types.RunMove, error) {
	mv, err := scanRunMove(s.Pool.QueryRow(ctx, `SELECT `+runMoveCols+` FROM run_moves WHERE id = $1`, id))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunMove{}, ErrNotFound
	}
	if err != nil {
		return types.RunMove{}, fmt.Errorf("store: get run move: %w", err)
	}
	return mv, nil
}

// ListRunMoves — see RunMoveStore.
func (s PG) ListRunMoves(ctx context.Context, runID uuid.UUID) ([]types.RunMove, error) {
	return s.queryRunMoves(ctx, `SELECT `+runMoveCols+` FROM run_moves WHERE run_id = $1 ORDER BY created_at, id`, runID)
}

// ListRunMovesFromRunner — see RunMoveStore.
func (s PG) ListRunMovesFromRunner(ctx context.Context, runnerID uuid.UUID) ([]types.RunMove, error) {
	return s.queryRunMoves(ctx, `SELECT `+runMoveCols+` FROM run_moves
		WHERE from_runner_id = $1 AND state = 'committed' AND cleanup = 'pending' ORDER BY created_at, id`, runnerID)
}

func (s PG) queryRunMoves(ctx context.Context, sql string, args ...any) ([]types.RunMove, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: list run moves: %w", err)
	}
	defer rows.Close()
	var out []types.RunMove
	for rows.Next() {
		mv, err := scanRunMove(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list run moves: %w", err)
		}
		out = append(out, mv)
	}
	return out, rows.Err()
}

// ListRunMovesDue — see RunMoveStore.
func (s PG) ListRunMovesDue(ctx context.Context, limit int) ([]RunMoveDue, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runMoveCols+`, COALESCE(heartbeat_at, created_at) < now() - $1::interval
		FROM run_moves
		WHERE state IN ('admitted','exporting','importing') OR (state = 'committed' AND released_at IS NULL)
			OR cleanup = 'pending' OR source_revoke = 'pending'
		ORDER BY created_at, id LIMIT $2`, RunMoveOrphanAfter.String(), limit)
	if err != nil {
		return nil, fmt.Errorf("store: list run moves due: %w", err)
	}
	defer rows.Close()
	var out []RunMoveDue
	for rows.Next() {
		var d RunMoveDue
		mv := &d.Move
		if err := rows.Scan(&mv.ID, &mv.RunID, &mv.State, &mv.PauseGen,
			&mv.FromPlacement, &mv.FromRunnerID, &mv.FromPoolID, &mv.FromExecutor, &mv.FromSandboxRef, &mv.FromEvidenceSource, &mv.FromTrustClass,
			&mv.ToPlacement, &mv.ToRunnerID, &mv.ToPoolID, &mv.ToExecutor, &mv.ToSandboxRef, &mv.ToEvidenceSource, &mv.ToTrustClass, &mv.ToTokenJTI,
			&mv.Cleanup, &mv.SourceRevoke, &mv.RecordingLevel, &mv.MovedBy, &mv.ManifestSHA256, &mv.Bytes,
			&mv.Skipped, &mv.Narrowed, &mv.Failure, &mv.HeartbeatAt, &mv.ReleasedAt, &mv.CreatedAt, &mv.UpdatedAt, &mv.CommittedAt,
			&d.Stale); err != nil {
			return nil, fmt.Errorf("store: list run moves due: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ListRunPlacements — see RunMoveStore.
func (s PG) ListRunPlacements(ctx context.Context, runID uuid.UUID) ([]types.RunPlacement, error) {
	rows, err := s.Pool.Query(ctx, `SELECT run_id, seq, cause, placement, runner_id, pool_id, executor, evidence_source,
		trust_class, recording_level, move_id, started_at FROM run_placements WHERE run_id = $1 ORDER BY seq`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list run placements: %w", err)
	}
	defer rows.Close()
	var out []types.RunPlacement
	for rows.Next() {
		var p types.RunPlacement
		if err := rows.Scan(&p.RunID, &p.Seq, &p.Cause, &p.Placement, &p.RunnerID, &p.PoolID, &p.Executor, &p.EvidenceSource,
			&p.TrustClass, &p.RecordingLevel, &p.MoveID, &p.StartedAt); err != nil {
			return nil, fmt.Errorf("store: list run placements: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// execOne runs a one-row conditional UPDATE and reports whether it changed the row.
func (s PG) execOne(ctx context.Context, what, sql string, args ...any) (bool, error) {
	tag, err := s.Pool.Exec(ctx, sql, args...)
	if err != nil {
		return false, fmt.Errorf("store: %s: %w", what, err)
	}
	return tag.RowsAffected() == 1, nil
}

// SetRunMovePauseGen — see RunMoveStore.
func (s PG) SetRunMovePauseGen(ctx context.Context, id uuid.UUID, gen int64) (bool, error) {
	return s.execOne(ctx, "set run move pause generation", `UPDATE run_moves SET pause_gen = $2, updated_at = now()
		WHERE id = $1 AND state IN ('admitted','exporting','importing')`, id, gen)
}

// SetRunMoveDestination — see RunMoveStore.
func (s PG) SetRunMoveDestination(ctx context.Context, mv types.RunMove) (bool, error) {
	return s.execOne(ctx, "set run move destination", `UPDATE run_moves SET to_placement = $2, to_runner_id = $3,
		to_pool_id = $4, to_executor = $5, to_sandbox_ref = $6, to_evidence_source = $7, to_trust_class = $8,
		to_token_jti = $9, narrowed = $10, updated_at = now()
		WHERE id = $1 AND state IN ('admitted','exporting','importing')`,
		mv.ID, mv.ToPlacement, mv.ToRunnerID, mv.ToPoolID, mv.ToExecutor, mv.ToSandboxRef, mv.ToEvidenceSource,
		mv.ToTrustClass, mv.ToTokenJTI, nullJSON(mv.Narrowed))
}

func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// AdvanceRunMove — see RunMoveStore.
func (s PG) AdvanceRunMove(ctx context.Context, id uuid.UUID, from, to types.MoveState) (bool, error) {
	if !from.InFlight() || !to.InFlight() {
		return false, fmt.Errorf("store: advance run move: %s to %s is not a step in flight", from, to)
	}
	return s.execOne(ctx, "advance run move", `UPDATE run_moves SET state = $3, updated_at = now() WHERE id = $1 AND state = $2`,
		id, from, to)
}

// StampRunMoveHeartbeat — see RunMoveStore.
func (s PG) StampRunMoveHeartbeat(ctx context.Context, id uuid.UUID) (bool, error) {
	return s.execOne(ctx, "stamp run move heartbeat", `UPDATE run_moves SET heartbeat_at = now(), updated_at = now()
		WHERE id = $1 AND released_at IS NULL AND state IN ('admitted','exporting','importing','committed')
			AND COALESCE(heartbeat_at, created_at) >= now() - $2::interval`, id, (RunMoveOrphanAfter - RunMoveHoldGuard).String())
}

// EndRunMove — see RunMoveStore.
func (s PG) EndRunMove(ctx context.Context, id uuid.UUID, state types.MoveState, failure string, cleanup types.MoveCleanup) (bool, error) {
	if (state != types.MoveRefused && state != types.MoveFailed) || failure == "" {
		return false, fmt.Errorf("store: end run move: %q with reason %q is not an end", state, failure)
	}
	return s.execOne(ctx, "end run move", `UPDATE run_moves SET state = $2, failure = $3, cleanup = $4, updated_at = now()
		WHERE id = $1 AND state IN ('admitted','exporting','importing')`, id, state, failure, cleanup)
}

// FailStaleRunMove — see RunMoveStore.
func (s PG) FailStaleRunMove(ctx context.Context, id uuid.UUID, failure string) (bool, error) {
	return s.execOne(ctx, "fail stale run move", `UPDATE run_moves SET state = 'failed', failure = $2, cleanup = 'pending',
		updated_at = now()
		WHERE id = $1 AND state IN ('admitted','exporting','importing') AND COALESCE(heartbeat_at, created_at) < now() - $3::interval`,
		id, failure, RunMoveOrphanAfter.String())
}

// ReleaseRunMove — see RunMoveStore.
func (s PG) ReleaseRunMove(ctx context.Context, id uuid.UUID, onlyStale bool) (bool, error) {
	return s.execOne(ctx, "release run move", `UPDATE run_moves SET released_at = now(), updated_at = now()
		WHERE id = $1 AND state = 'committed' AND released_at IS NULL
			AND (NOT $2 OR COALESCE(heartbeat_at, created_at) < now() - $3::interval)`, id, onlyStale, RunMoveOrphanAfter.String())
}

// SetRunMoveCleanup — see RunMoveStore.
func (s PG) SetRunMoveCleanup(ctx context.Context, id uuid.UUID, c types.MoveCleanup) error {
	_, err := s.execOne(ctx, "set run move cleanup", `UPDATE run_moves SET cleanup = $2, updated_at = now() WHERE id = $1`, id, c)
	return err
}

// SetRunMoveSourceRevoke — see RunMoveStore.
func (s PG) SetRunMoveSourceRevoke(ctx context.Context, id uuid.UUID, r types.MoveSourceRevoke) error {
	_, err := s.execOne(ctx, "set run move source revoke", `UPDATE run_moves SET source_revoke = $2, updated_at = now() WHERE id = $1`, id, r)
	return err
}

// runMoveSourceUnsettledSQL is commit's fence on the source's runner-bound rows ($1 the run): an unsettled pending
// action, an execution exit whose tail is incomplete, a containment-pending row or an open teardown hold.
const runMoveSourceUnsettledSQL = `SELECT (` + runnerActionUnsettledSQL + `)
	OR EXISTS (SELECT 1 FROM runner_execution_exits WHERE run_id = $1 AND NOT tail_complete)
	OR EXISTS (SELECT 1 FROM run_containment_pending WHERE run_id = $1)
	OR EXISTS (SELECT 1 FROM run_teardown_holds WHERE run_id = $1 AND cleared_at IS NULL)`

// CommitRunMove — see RunMoveStore. One transaction, under the move row's lock and then the run row's: the move
// must still be importing, the run RUNNING, not lost, and in the stopped pause at the move's generation, and the
// source settled. It then records the run's origin placement on its first move and appends the destination's,
// writes the destination's columns from the move row onto the run row (stamping token_renewed_at and opening
// the lineage with the destination's create-time jti), records each grant's delivery at the destination, closes
// the run's spool cursor and its pause settle record, and marks the move committed with the source revoke
// pending. A refusal is one of the ErrRunMove* errors and changes nothing.
func (s PG) CommitRunMove(ctx context.Context, c RunMoveCommit) error {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("store: commit run move: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed transaction is closed
	mv, err := scanRunMove(tx.QueryRow(ctx, `SELECT `+runMoveCols+` FROM run_moves WHERE id = $1 AND run_id = $2 FOR UPDATE`, c.MoveID, c.RunID))
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrRunMoveNotImporting
	}
	if err != nil {
		return fmt.Errorf("store: commit run move: %w", err)
	}
	if mv.State != types.MoveImporting {
		return ErrRunMoveNotImporting
	}
	if mv.ToTokenJTI == "" || mv.ToSandboxRef == "" {
		return fmt.Errorf("store: commit run move: the move recorded no destination sandbox or token")
	}
	var state string
	var lost, pausedAt *time.Time
	var reason string
	if err := tx.QueryRow(ctx, `SELECT state, lost_at, paused_at, paused_reason FROM agent_runs WHERE id = $1 FOR UPDATE`, c.RunID).
		Scan(&state, &lost, &pausedAt, &reason); err != nil {
		return fmt.Errorf("store: commit run move: %w", err)
	}
	switch {
	case state != string(types.RunRunning) || lost != nil:
		return ErrRunMoveRunEnded
	case reason != "stopped" || pausedAt == nil || pausedAt.UnixMicro() != mv.PauseGen:
		return ErrRunMovePauseChanged
	}
	var unsettled bool
	if err := tx.QueryRow(ctx, runMoveSourceUnsettledSQL, c.RunID).Scan(&unsettled); err != nil {
		return fmt.Errorf("store: commit run move: %w", err)
	}
	if unsettled {
		return ErrRunMoveSourceUnsettled
	}
	if err := commitRunMoveRows(ctx, tx, c, mv, *pausedAt); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// commitRunMoveRows is CommitRunMove's writes, once its conditions hold under the row locks.
func commitRunMoveRows(ctx context.Context, tx pgx.Tx, c RunMoveCommit, mv types.RunMove, pausedAt time.Time) error {
	if _, err := tx.Exec(ctx, `INSERT INTO run_placements (run_id, seq, cause, placement, runner_id, pool_id, executor,
			evidence_source, trust_class, recording_level, started_at)
		SELECT id, 1, 'origin', placement, runner_id, runner_pool_id, runner_pool_executor, evidence_source,
			COALESCE(runner_trust_class, ''), recording_level, created_at
		FROM agent_runs WHERE id = $1 AND NOT EXISTS (SELECT 1 FROM run_placements WHERE run_id = $1)`, c.RunID); err != nil {
		return fmt.Errorf("store: commit run move: origin placement: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO run_placements (run_id, seq, cause, placement, runner_id, pool_id, executor,
			evidence_source, trust_class, recording_level, move_id)
		SELECT $1, COALESCE(max(seq), 0) + 1, 'move', $2, $3, $4, $5, $6, $7, $8, $9 FROM run_placements WHERE run_id = $1`,
		c.RunID, mv.ToPlacement, mv.ToRunnerID, mv.ToPoolID, mv.ToExecutor, mv.ToEvidenceSource, mv.ToTrustClass,
		mv.RecordingLevel, mv.ID); err != nil {
		return fmt.Errorf("store: commit run move: placement: %w", err)
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs SET placement = $2, placement_filled = $3, runner_id = $4, runner_pool_id = $5,
			runner_pool_revision = $6, runner_pool_selection = $7, runner_pool_executor = $8, sandbox_ref = $9,
			evidence_source = $10, runner_trust_class = NULLIF($11, ''), sandbox_absent_at = NULL,
			token_renewed_at = now(), token_jti = $12, token_prev_jti = NULL, updated_at = now()
		WHERE id = $1 AND state = 'RUNNING' AND lost_at IS NULL AND paused_reason = 'stopped' AND paused_at = $13`,
		c.RunID, mv.ToPlacement, c.PlacementFilled, mv.ToRunnerID, mv.ToPoolID, c.PoolRevision, c.PoolSelection,
		mv.ToExecutor, mv.ToSandboxRef, mv.ToEvidenceSource, mv.ToTrustClass, mv.ToTokenJTI, pausedAt)
	if err != nil {
		return fmt.Errorf("store: commit run move: run row: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrRunMoveRunEnded
	}
	for grant, delivery := range c.Deliveries {
		if _, err := tx.Exec(ctx, `UPDATE credential_grants SET delivery = $3 WHERE id = $1 AND run_id = $2`, grant, c.RunID, delivery); err != nil {
			return fmt.Errorf("store: commit run move: grant delivery: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM runner_spool_cursors WHERE run_id = $1`, c.RunID); err != nil {
		return fmt.Errorf("store: commit run move: spool cursor: %w", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM run_pause_settles WHERE run_id = $1`, c.RunID); err != nil {
		return fmt.Errorf("store: commit run move: pause settle: %w", err)
	}
	if _, err := tx.Exec(ctx, `UPDATE run_moves SET state = 'committed', source_revoke = 'pending', manifest_sha256 = NULLIF($2, ''),
			bytes = $3, skipped = $4, heartbeat_at = now(), committed_at = now(), updated_at = now() WHERE id = $1`,
		mv.ID, c.ManifestSHA256, c.Bytes, c.Skipped); err != nil {
		return fmt.Errorf("store: commit run move: move row: %w", err)
	}
	return nil
}
