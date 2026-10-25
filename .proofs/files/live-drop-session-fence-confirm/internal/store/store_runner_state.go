// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrRunnerReconciliationRequired refuses READY without issuing an RPC from
// the checked STATE handler; page intake commits any containment action first.
var ErrRunnerReconciliationRequired = errors.New("runner lifecycle reconciliation required")

// RunnerContainment identifies the exact kept incarnation requiring H3's
// durable stop_proxy queue. Only an exact action_result may settle that action.
type RunnerContainment struct {
	RunnerID, RunID, Generation uuid.UUID
	Ref                         string
	LostAt                      time.Time
}

// reconcileRunnerStateRowTx validates one runner assertion against org
// authority. It does not renew credentials, bind handles, clear lost marks or
// finalize runs, and takes no run-operation lock or remote RPC.
func (s PG) reconcileRunnerStateRowTx(ctx context.Context, tx pgx.Tx, runnerID uuid.UUID, row runnerwire.RunState) (*RunnerContainment, bool, error) {
	run, err := runnerExecutionRun(ctx, tx, runnerID, row.RunID)
	if err != nil {
		return nil, false, err
	}
	current, err := readRunnerExecution(ctx, tx, row.RunID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return nil, false, err
	}
	if row.Execution != nil && (row.Execution.Generation != current.Generation || row.Execution.Operation != current.Operation || !row.Execution.Attempted || (current.ExecID != "" && current.ExecID != row.Execution.ExecID)) {
		return nil, false, ErrRunnerExecutionRefused
	}
	var lapsed bool
	if err = tx.QueryRow(ctx, `SELECT token_renewed_at < clock_timestamp()-$2::interval FROM agent_runs WHERE id=$1`, row.RunID, identity.RunTokenLapseAfter.String()).Scan(&lapsed); err != nil {
		return nil, false, err
	}
	var request *RunnerContainment
	if run.State == types.RunRunning && run.LostAt != nil && run.LostReason == types.LostOutage && row.SandboxPresent && row.ProxyStoppedAt == nil {
		request = &RunnerContainment{RunnerID: runnerID, RunID: run.ID, Generation: current.Generation, Ref: run.SandboxRef, LostAt: *run.LostAt}
	}
	// A timestamp is an assertion, never an ACK: H3's exact action reconciliation
	// alone clears pending_runner. Live missing sandboxes also require recovery.
	needsLoss := run.State == types.RunRunning && run.LostAt == nil && (lapsed || !row.SandboxPresent)
	return request, needsLoss, nil
}

// ListRunnerLapsedRuns is a bounded page for the offline callback. The loss
// claim rechecks all authority and DB lapse in MarkRunnerRunLost; this list is
// only a candidate scan and grants no authority to mutate a run.
func (s PG) ListRunnerLapsedRuns(ctx context.Context, runnerID, after uuid.UUID) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "runner lapsed runs", `SELECT `+runCols+` FROM agent_runs WHERE runner_id=$1 AND id>$2 AND placement='local' AND state='RUNNING' AND lost_at IS NULL AND token_renewed_at<clock_timestamp()-$3::interval ORDER BY id LIMIT 100`, []any{runnerID, after, identity.RunTokenLapseAfter.String()}, scanRun)
}

// MarkRunnerRunLost uses the existing loss CAS, with authenticated ownership
// checked in the same committing transaction. A fresh drop never claims loss.
func (s PG) MarkRunnerRunLost(ctx context.Context, runnerID, runID uuid.UUID, expectedRef string, at time.Time) (bool, error) {
	// Wait for the run row before the claim takes the runner's: runnerExecutionRun locks the runner row,
	// then the run row, and a wait there on a held run row would hold every write to the runner (its
	// reconnect's among them) behind that row's holder. The claim below still rechecks everything.
	if _, err := s.Pool.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
		return false, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return false, err
	}
	if expectedRef == "" || run.SandboxRef != expectedRef {
		return false, ErrRunnerExecutionRefused
	}
	tag, err := tx.Exec(ctx, markRunLostSQL, runID, at, string(types.LostOutage), string(types.RunRunning), identity.RunTokenLapseAfter.String(), string(types.LostReboot))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}

// RunnerSandboxGoneAfter is how long a run's sandbox must stay reported absent, from its first absent
// report to a later one that still reports it absent, before the run is ended lost (gone). The later
// report is timed by when it began: a fresh session's inventory (goneConfirmedSQL), or the database time
// read before a status check (probeConfirmedSQL). A refused runner reconnects at 1 s doubling (runnerwire
// reconnectDelay), so the sixth session at the latest is past it; one report never is.
const RunnerSandboxGoneAfter = 30 * time.Second

// goneClaimableSQL is TRUE for an agent_runs row (alias a) a gone mark may land on: RUNNING and holding
// a sandbox (HoldsSandboxSQL), with no containment report from its runner and no staged exit still to
// finish, each of which settles the run on its own path.
const goneClaimableSQL = `a.state='RUNNING' AND ` + HoldsSandboxSQL + `
	AND NOT EXISTS (SELECT 1 FROM run_containment_pending p WHERE p.run_id=a.id)
	AND NOT EXISTS (SELECT 1 FROM runner_execution_exits x WHERE x.run_id=a.id AND NOT x.tail_complete AND NOT x.receipt_only AND x.parked_at IS NULL)`

// goneConfirmedSQL is TRUE for an agent_runs row (alias a) whose sandbox runner $1's current inventory
// still reports absent at least $2 (RunnerSandboxGoneAfter) after its first absent report, and that a
// gone mark may land on. It reads o.needs_loss, so intakeRunnerStateRow runs runnerSandboxGoneTx after
// syncRunContainmentPendingTx (this report's containment row is the one goneClaimableSQL checks) and
// before its observation insert (the row's needs_loss carries the gone verdict).
const goneConfirmedSQL = `a.sandbox_absent_at IS NOT NULL AND ` + goneClaimableSQL + `
	AND EXISTS (SELECT 1 FROM runner_inventories i JOIN runner_inventory_observations o ON o.runner_id=i.runner_id
		WHERE i.runner_id=$1 AND o.run_id=a.id AND o.needs_loss AND i.created_at >= a.sandbox_absent_at + $2::interval)`

// runnerSandboxGoneTx records one STATE report of a known run's sandbox. Absent with no exit code,
// under the run's own ref, with no retirement or containment report and nothing else settling the run,
// it stamps agent_runs.sandbox_absent_at once and answers true, which refuses READY. Any other report
// clears the stamp, so only an unbroken series of absent reports confirms.
func runnerSandboxGoneTx(ctx context.Context, tx pgx.Tx, row runnerwire.RunState) (bool, error) {
	if !row.SandboxPresent && row.Ref != "" && row.Retirement == nil && row.ContainmentPending == nil {
		tag, err := tx.Exec(ctx, `UPDATE agent_runs a SET sandbox_absent_at=COALESCE(a.sandbox_absent_at, clock_timestamp())
			WHERE a.id=$1 AND a.sandbox_ref=$2 AND `+goneClaimableSQL, row.RunID, row.Ref)
		if err != nil || tag.RowsAffected() == 1 {
			return err == nil, err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE agent_runs SET sandbox_absent_at=NULL WHERE id=$1 AND sandbox_absent_at IS NOT NULL`, row.RunID)
	return false, err
}

// ListRunnerGoneRuns is the gone pass's bounded page: the runner's runs whose sandbox its current
// inventory confirmed gone. It grants no authority; MarkRunnerRunGone rechecks everything.
func (s PG) ListRunnerGoneRuns(ctx context.Context, runnerID, after uuid.UUID) ([]types.AgentRun, error) {
	return collect(ctx, s.Pool, "list", "runner gone runs",
		`SELECT `+runCols+` FROM agent_runs a WHERE a.runner_id=$1 AND a.id>$3 AND a.placement='local' AND `+goneConfirmedSQL+` ORDER BY a.id LIMIT 100`,
		[]any{runnerID, RunnerSandboxGoneAfter.String(), after}, scanRun)
}

// MarkRunnerRunGone marks a run lost (gone) for its authenticated runner, only while the runner's
// current inventory confirms its sandbox gone (goneConfirmedSQL). It clears a pause: the agent is not
// running. The lock order is MarkRunnerRunLost's.
func (s PG) MarkRunnerRunGone(ctx context.Context, runnerID, runID uuid.UUID, expectedRef string, at time.Time) (bool, error) {
	if _, err := s.Pool.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
		return false, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return false, err
	}
	if expectedRef == "" || run.SandboxRef != expectedRef {
		return false, ErrRunnerExecutionRefused
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs a SET lost_at=$3, lost_reason=$4, paused_at=NULL, paused_reason=''
		WHERE a.id=$5 AND a.runner_id=$1 AND `+goneConfirmedSQL, runnerID, RunnerSandboxGoneAfter.String(), at, string(types.LostGone), runID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}

// probeSessionSQL is TRUE for an agent_runs row (alias a) whose runner's current session began its
// inventory at or before $4, the database time read before a status check of the run's sandbox
// (RunnerProbeClock). Every check over the current session passes: every call over a session follows
// its BeginInventory (remote Hub.start runs fresh before AttachSession), and a resumed session keeps the
// row it began with. A check issued before it went over a superseded session, so its late reply, on this
// replica or another, records nothing.
const probeSessionSQL = `EXISTS (SELECT 1 FROM runner_inventories i WHERE i.runner_id=a.runner_id AND i.created_at <= $4::timestamptz)`

// probeConfirmedSQL is goneConfirmedSQL for a status check: TRUE for an agent_runs row (alias a) first
// reported absent at least $5 (RunnerSandboxGoneAfter) before $4, the database time read before the
// check that found it absent again over the runner's current session, that a gone mark may land on.
const probeConfirmedSQL = `a.sandbox_absent_at IS NOT NULL AND $4::timestamptz >= a.sandbox_absent_at + $5::interval AND ` + goneClaimableSQL

// RunnerProbeClock reads the database clock before a status check of a runner's sandbox (api
// probeRunnerSandbox). The check observes after it, so a report timed here is never later than what
// it saw, on the clock the absent stamp and the runner's inventories use.
func (s PG) RunnerProbeClock(ctx context.Context) (time.Time, error) {
	var at time.Time
	err := s.Pool.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at)
	return at, err
}

// NoteRunnerSandboxProbe records one status check of runner runnerID's run runID under ref, issued at
// database time issued (RunnerProbeClock). Absent with no exit code, over the runner's current session,
// on a run a gone mark may land on, it stamps agent_runs.sandbox_absent_at once (as runnerSandboxGoneTx
// does) and answers stamped, and due once the stamp is at least RunnerSandboxGoneAfter older than
// issued (MarkRunnerRunGoneProbed confirms). Any other answer over the current session clears the stamp,
// and a check over a superseded session changes nothing. A failed check is never recorded: the caller
// passes only an answer.
func (s PG) NoteRunnerSandboxProbe(ctx context.Context, runnerID, runID uuid.UUID, ref string, absent bool, issued time.Time) (due, stamped bool, err error) {
	if absent {
		err = s.Pool.QueryRow(ctx, `UPDATE agent_runs a SET sandbox_absent_at=COALESCE(a.sandbox_absent_at, clock_timestamp())
			WHERE a.id=$1 AND a.runner_id=$2 AND a.sandbox_ref=$3 AND a.placement='local' AND `+probeSessionSQL+` AND `+goneClaimableSQL+`
			RETURNING $4::timestamptz >= a.sandbox_absent_at + $5::interval`, runID, runnerID, ref, issued, RunnerSandboxGoneAfter.String()).Scan(&due)
		if err == nil {
			return due, true, nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return false, false, err
		}
	}
	_, err = s.Pool.Exec(ctx, `UPDATE agent_runs a SET sandbox_absent_at=NULL
		WHERE a.id=$1 AND a.runner_id=$2 AND a.sandbox_ref=$3 AND a.sandbox_absent_at IS NOT NULL AND `+probeSessionSQL, runID, runnerID, ref, issued)
	return false, false, err
}

// MarkRunnerRunGoneProbed is MarkRunnerRunGone for a confirmation by a status check: the mark lands only
// while probeConfirmedSQL holds for issued, the database time read before the check that found the
// sandbox absent again. The lock order is MarkRunnerRunLost's.
func (s PG) MarkRunnerRunGoneProbed(ctx context.Context, runnerID, runID uuid.UUID, expectedRef string, issued, at time.Time) (bool, error) {
	if _, err := s.Pool.Exec(ctx, `SELECT 1 FROM agent_runs WHERE id = $1 FOR UPDATE`, runID); err != nil {
		return false, err
	}
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // committed tx is already closed
	run, err := runnerExecutionRun(ctx, tx, runnerID, runID)
	if err != nil {
		return false, err
	}
	if expectedRef == "" || run.SandboxRef != expectedRef {
		return false, ErrRunnerExecutionRefused
	}
	tag, err := tx.Exec(ctx, `UPDATE agent_runs a SET lost_at=$6, lost_reason=$7, paused_at=NULL, paused_reason=''
		WHERE a.id=$1 AND a.runner_id=$2 AND a.sandbox_ref=$3 AND a.placement='local' AND `+probeConfirmedSQL,
		runID, runnerID, expectedRef, issued, RunnerSandboxGoneAfter.String(), at, string(types.LostGone))
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, tx.Commit(ctx)
}
