// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const testSpoolHash = "1111111111111111111111111111111111111111111111111111111111111111"

// endedRetirementRun is a COMPLETED runner run with an issued generation, a
// watermark row (capture, accepted end 7 when captured) and a spool cursor at
// seq 2: everything the producer requires.
func (f actionFixture) endedRetirementRun(t *testing.T, capture bool) (types.AgentRun, uuid.UUID) {
	t.Helper()
	run := f.run(t)
	gen := f.issue(t, run)
	if err := f.st.InsertRunnerOutputWatermark(t.Context(), run.ID, capture); err != nil {
		t.Fatal(err)
	}
	if capture {
		if err := f.st.AdvanceRunnerOutputWatermark(t.Context(), run.ID, 7); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO runner_spool_cursors (run_id, runner_id, spool_id, last_seq, recent_hashes)
		VALUES ($1, $2, $3, 2, $4)`, run.ID, f.rn.ID, uuid.New(), `{"1":"`+testSpoolHash[:63]+`0","2":"`+testSpoolHash+`"}`); err != nil {
		t.Fatal(err)
	}
	f.end(t, run)
	return run, gen
}

// candidates checks the producer's candidate runs are exactly want.
// reportRetireCapable is the caps report of a runner that settles retire_run, in its one inventory session.
func (f actionFixture) reportRetireCapable(t *testing.T) {
	t.Helper()
	session := f.rn.ID.String()
	if err := f.st.BeginRunnerInventory(t.Context(), f.rn.ID, session); err != nil {
		t.Fatal(err)
	}
	if current, err := f.st.RecordRunnerCaps(t.Context(), f.rn.ID, session, store.RunnerCapsReport{RetireCapable: true}, time.Now()); err != nil || !current {
		t.Fatalf("caps report: current = %v, %v", current, err)
	}
}

func (f actionFixture) candidates(t *testing.T, want ...uuid.UUID) {
	t.Helper()
	got, err := f.st.RunnerRetirementCandidates(t.Context())
	ids := make([]uuid.UUID, 0, len(got))
	for _, c := range got {
		if c.RunnerID != f.rn.ID {
			t.Fatalf("candidate %+v names another runner", c)
		}
		ids = append(ids, c.RunID)
	}
	if err != nil || !slices.Equal(ids, want) {
		t.Fatalf("candidates = %v %v, want %v", ids, err, want)
	}
}

func (f actionFixture) end(t *testing.T, run types.AgentRun) {
	t.Helper()
	if _, err := f.pool.Exec(t.Context(), `UPDATE agent_runs SET state = 'COMPLETED' WHERE id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
}

func (f actionFixture) advance(t *testing.T, runID uuid.UUID, want string) {
	t.Helper()
	got, err := f.st.AdvanceRunnerRetirement(t.Context(), runID, prepareSpoolTest)
	if err != nil || got != want {
		t.Fatalf("advance = %q %v, want %q", got, err, want)
	}
}

// tip is the newest action of the run's retirement obligation.
func (f actionFixture) tip(t *testing.T, runID uuid.UUID) types.RunnerPendingAction {
	t.Helper()
	page, _, err := f.st.ListPendingRunnerActions(t.Context(), f.rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range slices.Backward(page) {
		if a.RunID == runID {
			return a
		}
	}
	t.Fatalf("run %s has no open action", runID)
	return types.RunnerPendingAction{}
}

func (f actionFixture) result(t *testing.T, a types.RunnerPendingAction, outcome string) error {
	t.Helper()
	reason := ""
	if outcome == types.RunnerActionRefused {
		reason = "evidence disagrees"
	}
	_, _, err := f.st.RecordRunnerActionResult(t.Context(), f.rn.ID, a.RunID, a.ID, outcome, reason, time.Now(), prepareSpoolTest)
	return err
}

func (f actionFixture) retirementState(t *testing.T, runID uuid.UUID) string {
	t.Helper()
	var state string
	if err := f.pool.QueryRow(t.Context(), `SELECT state FROM runner_retirements WHERE run_id = $1`, runID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	return state
}

func (f actionFixture) retireRows(t *testing.T, runID uuid.UUID) int {
	return f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND kind = 'retire_run'`, runID)
}

// The producer opens nothing for a run without a watermark row, a spool
// cursor, a terminal state or a ref; with all of them it snapshots the live
// ref and queues the obligation's first kill, bound to the snapshot.
func TestPG_QueueRunnerRetirement_RequiresSnapshotWatermarkCursor(t *testing.T) {
	f := newActionFixture(t)
	bare := f.run(t)
	f.end(t, bare)
	f.advance(t, bare.ID, store.RetirementIdle)
	if err := f.st.InsertRunnerOutputWatermark(t.Context(), bare.ID, false); err != nil {
		t.Fatal(err)
	}
	f.advance(t, bare.ID, store.RetirementIdle) // no spool cursor: a zero-egress run is not retired
	legacy := f.run(t)                          // dispatched before the watermark table: a cursor, no watermark row
	if _, err := f.pool.Exec(t.Context(), `INSERT INTO runner_spool_cursors (run_id, runner_id, spool_id) VALUES ($1, $2, $3)`, legacy.ID, f.rn.ID, uuid.New()); err != nil {
		t.Fatal(err)
	}
	f.end(t, legacy)
	f.advance(t, legacy.ID, store.RetirementIdle)
	live, _ := f.endedRetirementRun(t, false)
	if _, err := f.pool.Exec(t.Context(), `UPDATE agent_runs SET state = 'RUNNING' WHERE id = $1`, live.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(t, live.ID, store.RetirementIdle)
	if n := f.count(t, `SELECT count(*) FROM runner_retirements`); n != 0 {
		t.Fatalf("%d snapshots for ineligible runs", n)
	}

	run, gen := f.endedRetirementRun(t, false)
	// A runner whose caps never said retire (older than 0.9.3) has no
	// candidates; once it says so, the run is one.
	f.candidates(t)
	f.reportRetireCapable(t)
	f.candidates(t, run.ID)
	f.advance(t, run.ID, store.RetirementKill)
	f.candidates(t) // the kill waits for the runner: not selected again until answered
	kill := f.tip(t, run.ID)
	var obligation uuid.UUID
	if err := f.pool.QueryRow(t.Context(), `SELECT obligation_id FROM runner_retirements WHERE run_id = $1 AND state = 'kill' AND ref = $2 AND generation = $3`,
		run.ID, run.SandboxRef, gen).Scan(&obligation); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	if kill.Kind != types.RunnerActionKill || kill.Ref != run.SandboxRef || kill.Generation != gen || kill.ObligationID != obligation || kill.Retirement != nil {
		t.Fatalf("first kill = %+v, want bound to the snapshot %s", kill, obligation)
	}
	f.advance(t, run.ID, store.RetirementIdle) // the kill is still open
	if _, _, err := f.st.QueueRunnerAction(t.Context(), types.RunnerPendingAction{ID: uuid.New(), RunnerID: f.rn.ID, RunID: run.ID,
		Kind: types.RunnerActionRetire, Ref: run.SandboxRef, Retirement: &types.RunnerRetirement{ID: uuid.New()}}); !errors.Is(err, store.ErrRunnerActionRefused) {
		t.Fatalf("QueueRunnerAction(retire_run) = %v, want refused", err)
	}
}

// The retire_run exists only after the obligation's kill succeeds; a deferred
// kill gets a successor in its own phase, and its success settles the
// deferred predecessor. Results bind to the snapshot once the ref is cleared.
func TestPG_Retirement_RetireQueuedOnlyAfterKillSuccess(t *testing.T) {
	f := newActionFixture(t)
	run, gen := f.endedRetirementRun(t, false)
	if err := f.st.ClearSandboxRefRetaining(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := f.st.GetRun(t.Context(), run.ID); err != nil || got.SandboxRef != "" {
		t.Fatalf("ref after the clear = %q %v", got.SandboxRef, err)
	}
	if f.retirementState(t, run.ID) != "kill" {
		t.Fatal("the clear left no snapshot")
	}
	f.advance(t, run.ID, store.RetirementKill)
	first := f.tip(t, run.ID)
	if err := f.result(t, first, types.RunnerActionDeferred); err != nil {
		t.Fatalf("deferred kill result against the snapshot: %v", err)
	}
	if f.retireRows(t, run.ID) != 0 {
		t.Fatal("a retire_run was queued beside a deferred kill")
	}
	f.advance(t, run.ID, store.RetirementSuccessor)
	successor := f.tip(t, run.ID)
	if successor.Kind != types.RunnerActionKill || successor.ObligationID != first.ObligationID || successor.ID == first.ID {
		t.Fatalf("successor = %+v", successor)
	}
	if err := f.result(t, successor, types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id = $1 AND settlement = 'chain' AND settled_by = $2`, first.ID, successor.ID); n != 1 {
		t.Fatal("the successor's success did not settle the deferred kill")
	}
	f.advance(t, run.ID, store.RetirementRetire)
	retire := f.tip(t, run.ID)
	want := types.RunnerRetirement{ID: retire.Retirement.ID, Generation: gen, NoOutput: true,
		Spool: types.RunnerSpoolReceipt{SpoolID: retire.Retirement.Spool.SpoolID, Seq: 2, Hash: testSpoolHash}}
	if retire.Kind != types.RunnerActionRetire || retire.Retirement == nil || *retire.Retirement != want || retire.ObligationID != first.ObligationID {
		t.Fatalf("retire_run = %+v %+v, want receipt %+v", retire, retire.Retirement, want)
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.retire' AND outcome = 'success' AND run_id = $1`, run.ID); n != 1 {
		t.Fatalf("%d runner.retire rows, want 1", n)
	}
	f.advance(t, run.ID, store.RetirementIdle)
	if err := f.result(t, retire, types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	if f.retirementState(t, run.ID) != "settled" {
		t.Fatal("a successful retire_run left the obligation open")
	}
	if has, err := f.st.RunHasPendingRunnerAction(t.Context(), run.ID); err != nil || has {
		t.Fatalf("fence after settlement = %v %v", has, err)
	}
	f.reportRetireCapable(t)
	f.candidates(t) // a settled run is never a candidate
}

// A deferred retire_run's successor carries a recomputed receipt: the
// watermark may have moved since.
func TestPG_Retirement_DeferredSuccessorSettlesPredecessors(t *testing.T) {
	f := newActionFixture(t)
	run, _ := f.endedRetirementRun(t, true)
	f.advance(t, run.ID, store.RetirementKill)
	if err := f.result(t, f.tip(t, run.ID), types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	f.advance(t, run.ID, store.RetirementRetire)
	first := f.tip(t, run.ID)
	if first.Retirement.OutputEnd != 7 || first.Retirement.NoOutput {
		t.Fatalf("receipt = %+v, want the watermark's end 7", first.Retirement)
	}
	if err := f.result(t, first, types.RunnerActionDeferred); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AdvanceRunnerOutputWatermark(t.Context(), run.ID, 9); err != nil {
		t.Fatal(err)
	}
	if err := f.st.AdvanceRunnerOutputWatermark(t.Context(), run.ID, 3); err != nil { // a late duplicate never lowers it
		t.Fatal(err)
	}
	f.advance(t, run.ID, store.RetirementSuccessor)
	second := f.tip(t, run.ID)
	if second.Kind != types.RunnerActionRetire || second.Retirement.OutputEnd != 9 || second.Retirement.ID == first.Retirement.ID {
		t.Fatalf("successor receipt = %+v", second.Retirement)
	}
	if err := f.result(t, second, types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	if f.retirementState(t, run.ID) != "settled" || f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id = $1 AND settled_by = $2`, first.ID, second.ID) != 1 {
		t.Fatal("the successor's success did not settle the obligation and its predecessor")
	}
}

// A refused tip closes the obligation for good; the successor bound abandons
// it, audited, and its deferred actions stop fencing the run. Either way, and
// after a settlement, a second producer pass queues nothing.
func TestPG_Retirement_ClosedRowIsNeverRequeued(t *testing.T) {
	f := newActionFixture(t)

	refused, _ := f.endedRetirementRun(t, false)
	f.advance(t, refused.ID, store.RetirementKill)
	if err := f.result(t, f.tip(t, refused.ID), types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	f.advance(t, refused.ID, store.RetirementRetire)
	if err := f.result(t, f.tip(t, refused.ID), types.RunnerActionRefused); err != nil {
		t.Fatal(err)
	}
	if f.retirementState(t, refused.ID) != "refused" {
		t.Fatal("a refused retire_run left the obligation open")
	}

	exhausted, _ := f.endedRetirementRun(t, false)
	f.advance(t, exhausted.ID, store.RetirementKill)
	if err := f.result(t, f.tip(t, exhausted.ID), types.RunnerActionDeferred); err != nil {
		t.Fatal(err)
	}
	if _, err := f.pool.Exec(t.Context(), `UPDATE runner_retirements SET successors = $2 WHERE run_id = $1`, exhausted.ID, store.RetirementSuccessorBound); err != nil {
		t.Fatal(err)
	}
	f.advance(t, exhausted.ID, store.RetirementAbandoned)
	if f.retirementState(t, exhausted.ID) != "abandoned" ||
		f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE run_id = $1 AND settlement = 'abandoned'`, exhausted.ID) != 1 ||
		f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.retire' AND outcome = 'failure' AND run_id = $1 AND data->>'reason' = 'deferred_exhausted'`, exhausted.ID) != 1 {
		t.Fatal("the bound did not abandon the obligation with its audit row")
	}
	if has, err := f.st.RunHasPendingRunnerAction(t.Context(), exhausted.ID); err != nil || has {
		t.Fatalf("an abandoned obligation still fences its run: %v %v", has, err)
	}

	settled, _ := f.endedRetirementRun(t, false)
	f.advance(t, settled.ID, store.RetirementKill)
	if err := f.result(t, f.tip(t, settled.ID), types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	f.advance(t, settled.ID, store.RetirementRetire)
	if err := f.result(t, f.tip(t, settled.ID), types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}

	before := f.count(t, `SELECT count(*) FROM runner_pending_actions`)
	for _, run := range []types.AgentRun{refused, exhausted, settled} {
		f.advance(t, run.ID, store.RetirementIdle)
	}
	if after := f.count(t, `SELECT count(*) FROM runner_pending_actions`); after != before {
		t.Fatalf("a closed obligation was requeued: %d actions, then %d", before, after)
	}
}

// Once the ref is cleared, only a kill or retire_run that matches its run's
// open snapshot (run and runner, obligation, phase, ref, generation) is
// accepted; a runner refusal is always recorded.
func TestPG_RecordRunnerActionResult_SnapshotBindingOnlyForRetirementKinds(t *testing.T) {
	f := newActionFixture(t)
	run, gen := f.endedRetirementRun(t, false)
	if err := f.st.ClearSandboxRefRetaining(t.Context(), run.ID); err != nil {
		t.Fatal(err)
	}
	f.advance(t, run.ID, store.RetirementKill)
	kill := f.tip(t, run.ID)
	// stray inserts an action outside the producer; refusedStray checks the
	// org refuses its success and removes it, so it fences nothing after.
	stray := func(kind types.RunnerActionKind, obligation uuid.UUID) types.RunnerPendingAction {
		t.Helper()
		id := uuid.New()
		var proof []byte
		if kind == types.RunnerActionRetire {
			proof, _ = json.Marshal(types.RunnerRetirement{ID: uuid.New()})
		}
		if _, err := f.pool.Exec(t.Context(), `INSERT INTO runner_pending_actions (id, runner_id, run_id, kind, ref, binding_version, owner, execution_generation, proof, obligation_id)
			VALUES ($1, $2, $3, $4, $5, 1, $6, $7, $8, $9)`, id, f.rn.ID, run.ID, string(kind), kill.Ref, f.rn.Owner, gen, proof, obligation); err != nil {
			t.Fatal(err)
		}
		return types.RunnerPendingAction{ID: id, RunID: run.ID}
	}
	refusedStray := func(what string, kind types.RunnerActionKind, obligation uuid.UUID) {
		t.Helper()
		a := stray(kind, obligation)
		if err := f.result(t, a, types.RunnerActionSuccess); !errors.Is(err, store.ErrRunnerActionRefused) {
			t.Fatalf("%s = %v, want refused", what, err)
		}
		if _, err := f.pool.Exec(t.Context(), `DELETE FROM runner_pending_actions WHERE id = $1`, a.ID); err != nil {
			t.Fatal(err)
		}
	}
	refusedStray("an end against the snapshot", types.RunnerActionEnd, kill.ObligationID)
	// A second open kill cannot exist beside the first (one open action per
	// kind), so the obligation mismatch is checked once the first is applied.
	if err := f.result(t, kill, types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	refusedStray("a kill of another obligation", types.RunnerActionKill, uuid.New())
	f.advance(t, run.ID, store.RetirementRetire)
	refusedStray("a kill in the retire phase", types.RunnerActionKill, kill.ObligationID)
	retire := f.tip(t, run.ID)
	if err := f.result(t, retire, types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	late := stray(types.RunnerActionRetire, kill.ObligationID)
	if err := f.result(t, late, types.RunnerActionSuccess); !errors.Is(err, store.ErrRunnerActionRefused) {
		t.Fatalf("a success after the obligation closed = %v, want refused", err)
	}
	if err := f.result(t, late, types.RunnerActionRefused); err != nil {
		t.Fatalf("a runner refusal against a settled snapshot = %v, want recorded", err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_pending_actions WHERE id = $1 AND applied_at IS NOT NULL AND outcome = 'refused'`, late.ID); n != 1 {
		t.Fatal("the refusal was not recorded")
	}
	if n := f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.action.refuse' AND target = $1`, late.ID.String()); n != 1 {
		t.Fatal("the refusal has no runner.action.refuse row")
	}
	if f.retirementState(t, run.ID) != "settled" {
		t.Fatal("a refusal reopened or re-closed a settled snapshot")
	}
}

// A revoked runner's open retirement is abandoned, audited.
func TestPG_Retirement_RevokedRunnerAbandons(t *testing.T) {
	f := newActionFixture(t)
	run, _ := f.endedRetirementRun(t, false)
	f.advance(t, run.ID, store.RetirementKill)
	if _, err := f.st.RevokeRunner(t.Context(), f.rn.ID, time.Now(), "admin", ""); err != nil {
		t.Fatal(err)
	}
	if n, err := f.st.AbandonRevokedRunnerRetirements(t.Context(), prepareSpoolTest); err != nil || n != 1 {
		t.Fatalf("abandoned %d %v, want 1", n, err)
	}
	if f.retirementState(t, run.ID) != "abandoned" ||
		f.count(t, `SELECT count(*) FROM audit_events WHERE action = 'runner.retire' AND outcome = 'failure' AND data->>'reason' = 'runner_revoked'`) != 1 {
		t.Fatal("the revoked runner's retirement was not abandoned with its audit row")
	}
}

// An exit that reaches the org after its run ended is kept receipt-only: it
// names the exit in the retirement receipt, never enters exit work, and a
// resent different exit leaves it unchanged.
func TestPG_StageRunnerExit_TerminalReceiptOnlyNotInWork(t *testing.T) {
	f := newActionFixture(t)
	run, gen := f.endedRetirementRun(t, false)
	exit := runnerwire.AgentExit{Generation: gen, ExecID: "agent", Ref: run.SandboxRef, Code: 0, ObservedAt: time.Now().UTC().Truncate(time.Microsecond)}
	if _, err := f.pool.Exec(t.Context(), `UPDATE runner_executions SET exec_id = 'agent' WHERE run_id = $1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if err := f.st.StageRunnerExit(t.Context(), f.rn.ID, run.ID, exit); err != nil {
		t.Fatal(err)
	}
	changed := exit
	changed.Code = 1
	if err := f.st.StageRunnerExit(t.Context(), f.rn.ID, run.ID, changed); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("a different exit for the same generation = %v, want refused", err)
	}
	if n := f.count(t, `SELECT count(*) FROM runner_execution_exits WHERE run_id = $1 AND receipt_only AND accepted_at IS NULL`, run.ID); n != 1 {
		t.Fatal("no receipt-only exit row")
	}
	work, err := f.st.ListRunnerExitWork(t.Context(), uuid.Nil)
	if err != nil || len(work) != 0 {
		t.Fatalf("exit work = %+v %v, want none", work, err)
	}
	f.advance(t, run.ID, store.RetirementKill)
	if err := f.result(t, f.tip(t, run.ID), types.RunnerActionSuccess); err != nil {
		t.Fatal(err)
	}
	f.advance(t, run.ID, store.RetirementRetire)
	receipt := f.tip(t, run.ID).Retirement
	var hash string
	if err := f.pool.QueryRow(t.Context(), `SELECT payload_hash FROM runner_execution_exits WHERE run_id = $1`, run.ID).Scan(&hash); err != nil {
		t.Fatal(err)
	}
	if receipt.ExitHash != hash || hash == "" || receipt.ExecID != "agent" {
		t.Fatalf("receipt = %+v, want the stored exit's hash %s", receipt, hash)
	}
}
