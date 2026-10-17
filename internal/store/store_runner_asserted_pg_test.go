// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func assertedRow(rn types.Runner, runID uuid.UUID) runnerwire.RunState {
	return runnerwire.RunState{RunID: runID, Ref: "runner:" + rn.ID.String() + "/local-" + runID.String(), Generation: uuid.New(), SandboxPresent: true}
}

// finalInventory begins a fresh session and submits rows as one Final page.
func finalInventory(t *testing.T, st store.PG, rn types.Runner, session string, rows ...runnerwire.RunState) store.RunnerInventoryResult {
	t.Helper()
	if err := st.BeginRunnerInventory(t.Context(), rn.ID, session); err != nil {
		t.Fatal(err)
	}
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b runnerwire.RunState) int { return bytes.Compare(a.RunID[:], b.RunID[:]) })
	out, err := st.IntakeRunnerStatePage(t.Context(), rn.ID, session, runnerwire.StatePage{InventoryID: uuid.New(), Runs: rows, Final: true})
	if err != nil {
		t.Fatalf("intake: %v", err)
	}
	return out
}

func assertedActions(t *testing.T, st store.PG, rn types.Runner, runID uuid.UUID) []types.RunnerPendingAction {
	t.Helper()
	page, _, err := st.ListPendingRunnerActions(t.Context(), rn.ID)
	if err != nil {
		t.Fatal(err)
	}
	var out []types.RunnerPendingAction
	for _, a := range page {
		if a.RunID == runID {
			out = append(out, a)
		}
	}
	return out
}

// The probe that found the gap: an erased org run in a STATE page refused the
// whole page, stranding every unrelated run. It is now contained per run.
func TestPG_RunnerErasedRunIsContainedWithoutRefusingThePage(t *testing.T) {
	pool, st, rn, run, _ := spoolPGFixture(t)
	ctx := t.Context()
	healthy := inventoryRun(t, st, rn)
	session := uuid.NewString()
	if err := st.BeginRunnerInventory(ctx, rn.ID, session); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	erased := assertedRow(rn, run.ID)
	rows := []runnerwire.RunState{erased, {RunID: healthy.ID, SandboxPresent: true}}
	slices.SortFunc(rows, func(a, b runnerwire.RunState) int { return bytes.Compare(a.RunID[:], b.RunID[:]) })
	out, err := st.IntakeRunnerStatePage(ctx, rn.ID, session, runnerwire.StatePage{InventoryID: uuid.New(), Runs: rows, Final: true})
	if err != nil || !out.Complete || !out.NeedsReconciliation || out.AssertedCapHeld {
		t.Fatalf("erased run page = %+v, %v", out, err)
	}
	actions := assertedActions(t, st, rn, run.ID)
	if len(actions) != 2 {
		t.Fatalf("asserted actions = %+v", actions)
	}
	for i, kind := range []types.RunnerActionKind{types.RunnerActionKill, types.RunnerActionQuarantine} {
		a := actions[i]
		if a.Kind != kind || !a.Asserted || !a.Bound || a.Owner != "" || a.Generation != uuid.Nil || a.Ref != erased.Ref {
			t.Fatalf("action %d = %+v", i, a)
		}
	}
	if q := actions[1].Quarantine; q == nil || !q.Asserted || q.Generation != erased.Generation {
		t.Fatalf("quarantine proof = %+v", q)
	}
	// Re-asserting the subject reuses its rows.
	if out := finalInventory(t, st, rn, uuid.NewString(), erased, runnerwire.RunState{RunID: healthy.ID, SandboxPresent: true}); !out.NeedsReconciliation {
		t.Fatalf("re-assertion = %+v", out)
	}
	var rowsNow int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pending_actions WHERE asserted_run_id=$1`, run.ID).Scan(&rowsNow); err != nil || rowsNow != 2 {
		t.Fatalf("re-assertion grew the table to %d rows (%v)", rowsNow, err)
	}
	if fenced, err := st.RunHasPendingRunnerAction(ctx, healthy.ID); err != nil || fenced {
		t.Fatalf("asserted subject fenced an unrelated run: %v %v", fenced, err)
	}
	// Results are recorded without any run row; then the quarantined run
	// leaves the inventory and the unrelated run converges.
	if _, _, err := st.RecordRunnerActionResult(ctx, rn.ID, healthy.ID, actions[0].ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); err == nil {
		t.Fatal("asserted result accepted for another run")
	}
	for _, a := range actions {
		if _, first, err := st.RecordRunnerActionResult(ctx, rn.ID, run.ID, a.ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); err != nil || !first {
			t.Fatalf("asserted %s result: %v %v", a.Kind, first, err)
		}
	}
	if out := finalInventory(t, st, rn, uuid.NewString(), runnerwire.RunState{RunID: healthy.ID, SandboxPresent: true}); out.NeedsReconciliation || !out.Complete {
		t.Fatalf("converged inventory = %+v", out)
	}
}

// An asserted subject's ref must carry the authenticated runner's prefix.
func TestPG_RunnerAssertedSubjectRefusesAForeignRef(t *testing.T) {
	_, st, rn, _, _ := spoolPGFixture(t)
	row := assertedRow(rn, uuid.New())
	row.Ref = "runner:" + uuid.NewString() + "/local"
	session := uuid.NewString()
	if err := st.BeginRunnerInventory(t.Context(), rn.ID, session); err != nil {
		t.Fatal(err)
	}
	if _, err := st.IntakeRunnerStatePage(t.Context(), rn.ID, session, runnerwire.StatePage{InventoryID: uuid.New(), Runs: []runnerwire.RunState{row}, Final: true}); err == nil {
		t.Fatal("asserted subject with another runner's ref accepted")
	}
}

// The cap counts every distinct asserted subject, settled ones included.
func TestPG_RunnerAssertedSubjectCapCountsSettledSubjects(t *testing.T) {
	pool, st, rn, _, _ := spoolPGFixture(t)
	ctx := t.Context()
	rows := make([]runnerwire.RunState, types.RunnerAssertedSubjectCap)
	for i := range rows {
		rows[i] = assertedRow(rn, uuid.New())
	}
	if out := finalInventory(t, st, rn, uuid.NewString(), rows...); out.AssertedCapHeld {
		t.Fatalf("cap reached early: %+v", out)
	}
	// Settle every subject: settled subjects still count.
	if _, err := pool.Exec(ctx, `UPDATE runner_pending_actions SET applied_at=clock_timestamp(), outcome='success', observed_at=clock_timestamp() WHERE runner_id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	extra := assertedRow(rn, uuid.New())
	if out := finalInventory(t, st, rn, uuid.NewString(), extra); !out.AssertedCapHeld || !out.NeedsReconciliation {
		t.Fatalf("101st subject = %+v", out)
	}
	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pending_actions WHERE asserted_run_id=$1`, extra.RunID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("over-cap subject queued %d rows (%v)", n, err)
	}
	// A subject already within the cap is still re-asserted without growth.
	if out := finalInventory(t, st, rn, uuid.NewString(), rows[0]); out.AssertedCapHeld {
		t.Fatalf("existing subject held by the cap: %+v", out)
	}
}

// The database binds asserted rows: no owner, no execution generation, only
// kill/stop_proxy/quarantine_run, an asserted quarantine proof, and immutable
// subject and chain columns.
func TestPG_RunnerAssertedRowsAreBoundAndImmutable(t *testing.T) {
	pool, _, rn, run, _ := spoolPGFixture(t)
	ctx := t.Context()
	ref := "runner:" + rn.ID.String() + "/local"
	asserted := uuid.New()
	for name, q := range map[string]string{
		"asserted with owner":       `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner) VALUES ($1,$2,'asserted',$3,'kill',$4,1,'someone')`,
		"asserted end":              `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner) VALUES ($1,$2,'asserted',$3,'end',$4,1,'')`,
		"asserted generation":       `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner, execution_generation) VALUES ($1,$2,'asserted',$3,'kill',$4,1,'',gen_random_uuid())`,
		"asserted unflagged proof":  `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner, proof) VALUES ($1,$2,'asserted',$3,'quarantine_run',$4,1,'','{"id":"00000000-0000-0000-0000-000000000001","generation":"00000000-0000-0000-0000-000000000000"}')`,
		"asserted with a run row":   `INSERT INTO runner_pending_actions (id, runner_id, run_id, subject, asserted_run_id, kind, ref, binding_version, owner) VALUES ($1,$2,'` + run.ID.String() + `','asserted',$3,'kill',$4,1,'')`,
		"run subject without a run": `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner) VALUES ($1,$2,'run',$3,'kill',$4,1,'o')`,
	} {
		if _, err := pool.Exec(ctx, q, uuid.New(), rn.ID, asserted, ref); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A quarantine bound to a real run never carries the asserted flag.
	if _, err := pool.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, run_id, subject, kind, ref, binding_version, owner, proof)
		VALUES ($1,$2,$3,'run','quarantine_run',$4,1,'o',$5)`, uuid.New(), rn.ID, run.ID, ref,
		`{"id":"`+uuid.NewString()+`","generation":"`+uuid.Nil.String()+`","asserted":true}`); err == nil {
		t.Error("a run-bound quarantine with an asserted proof accepted")
	}
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner, applied_at, outcome, observed_at)
		VALUES ($1,$2,'asserted',$3,'kill',$4,1,'',now(),'deferred',now())`, id, rn.ID, asserted, ref); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner)
		VALUES ($1,$2,'asserted',$3,'kill',$4,1,'')`, uuid.New(), rn.ID, asserted, ref); err == nil {
		t.Error("a second chain root for the same asserted subject accepted")
	}
	for name, q := range map[string]string{
		"subject":        `UPDATE runner_pending_actions SET subject='run', run_id='` + run.ID.String() + `', asserted_run_id=NULL WHERE id=$1`,
		"asserted run":   `UPDATE runner_pending_actions SET asserted_run_id=gen_random_uuid() WHERE id=$1`,
		"obligation":     `UPDATE runner_pending_actions SET obligation_id=gen_random_uuid() WHERE id=$1`,
		"superseded off": `UPDATE runner_pending_actions SET superseded=false WHERE id=$1 AND superseded`,
	} {
		if name == "superseded off" {
			if _, err := pool.Exec(ctx, `UPDATE runner_pending_actions SET superseded=true WHERE id=$1`, id); err != nil {
				t.Fatal(err)
			}
		}
		if tag, err := pool.Exec(ctx, q, id); err == nil && tag.RowsAffected() > 0 {
			t.Errorf("%s changed after insert", name)
		}
	}
}

// A deferred asserted action gets one successor per completed inventory, with
// the identical binding and proof; the successor's success settles the chain.
func TestPG_RunnerAssertedDeferredContinuesOncePerCompletedInventory(t *testing.T) {
	pool, st, rn, _, _ := spoolPGFixture(t)
	ctx := t.Context()
	row := assertedRow(rn, uuid.New())
	finalInventory(t, st, rn, uuid.NewString(), row)
	actions := assertedActions(t, st, rn, row.RunID)
	if len(actions) != 2 {
		t.Fatalf("asserted actions = %+v", actions)
	}
	quarantine := actions[1]
	if _, _, err := st.RecordRunnerActionResult(ctx, rn.ID, row.RunID, actions[0].ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordRunnerActionResult(ctx, rn.ID, row.RunID, quarantine.ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	next := uuid.NewString()
	if n, err := st.ReissueDeferredAssertedRunnerActions(ctx, rn.ID, next); err != nil || n != 1 {
		t.Fatalf("reissue after a completed inventory = %d, %v", n, err)
	}
	if n, err := st.ReissueDeferredAssertedRunnerActions(ctx, rn.ID, next); err != nil || n != 0 {
		t.Fatalf("second reissue for the same inventory = %d, %v", n, err)
	}
	successor := assertedActions(t, st, rn, row.RunID)
	if len(successor) != 1 || successor[0].ID == quarantine.ID || successor[0].ObligationID != quarantine.ID ||
		successor[0].Quarantine == nil || *successor[0].Quarantine != *quarantine.Quarantine || successor[0].Ref != quarantine.Ref {
		t.Fatalf("successor = %+v", successor)
	}
	// The successor is deferred too, but its session's inventory never
	// completed: no further successor until one does.
	if err := st.BeginRunnerInventory(ctx, rn.ID, next); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordRunnerActionResult(ctx, rn.ID, row.RunID, successor[0].ID, types.RunnerActionDeferred, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	if n, err := st.ReissueDeferredAssertedRunnerActions(ctx, rn.ID, uuid.NewString()); err != nil || n != 0 {
		t.Fatalf("reissue after an incomplete inventory = %d, %v", n, err)
	}
	finalInventory(t, st, rn, uuid.NewString())
	if n, err := st.ReissueDeferredAssertedRunnerActions(ctx, rn.ID, uuid.NewString()); err != nil || n != 1 {
		t.Fatalf("reissue after the next completed inventory = %d, %v", n, err)
	}
	last := assertedActions(t, st, rn, row.RunID)
	if len(last) != 1 {
		t.Fatalf("third link = %+v", last)
	}
	if _, _, err := st.RecordRunnerActionResult(ctx, rn.ID, row.RunID, last[0].ID, types.RunnerActionSuccess, "", time.Now(), prepareSpoolTest); err != nil {
		t.Fatal(err)
	}
	var settled int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pending_actions WHERE obligation_id=$1 AND settlement='chain' AND settled_by=$2`,
		quarantine.ID, last[0].ID).Scan(&settled); err != nil || settled != 2 {
		t.Fatalf("chain settled %d deferred links, want 2 (%v)", settled, err)
	}
}

// Asserted rows are purged RunnerAssertedEvidenceTTL after the quarantine
// succeeded; a recent quarantine is kept.
func TestPG_RunnerAssertedSubjectsPurgedAfterRetention(t *testing.T) {
	pool, st, rn, _, _ := spoolPGFixture(t)
	ctx := t.Context()
	insert := func(runID uuid.UUID, age time.Duration) {
		t.Helper()
		ref := "runner:" + rn.ID.String() + "/local-" + runID.String()
		at := time.Now().Add(-age)
		if _, err := pool.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner, applied_at, outcome, observed_at)
			VALUES ($1,$2,'asserted',$3,'kill',$4,1,'',$5,'success',$5)`, uuid.New(), rn.ID, runID, ref, at); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO runner_pending_actions (id, runner_id, subject, asserted_run_id, kind, ref, binding_version, owner, proof, applied_at, outcome, observed_at)
			VALUES ($1,$2,'asserted',$3,'quarantine_run',$4,1,'',$5,$6,'success',$6)`, uuid.New(), rn.ID, runID, ref, `{"id":"`+uuid.NewString()+`","generation":"`+uuid.Nil.String()+`","asserted":true}`, at); err != nil {
			t.Fatal(err)
		}
	}
	old, recent := uuid.New(), uuid.New()
	insert(old, types.RunnerAssertedEvidenceTTL+time.Hour)
	insert(recent, types.RunnerAssertedEvidenceTTL-time.Hour)
	if n, err := st.PurgeAssertedRunnerSubjects(ctx); err != nil || n != 2 {
		t.Fatalf("purged %d rows, want the old subject's 2 (%v)", n, err)
	}
	var left int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM runner_pending_actions WHERE asserted_run_id=$1`, recent).Scan(&left); err != nil || left != 2 {
		t.Fatalf("recent subject kept %d rows (%v)", left, err)
	}
}
