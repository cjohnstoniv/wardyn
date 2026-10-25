// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// finalPage begins a fresh inventory session and takes rows as its one Final page.
func finalPage(t *testing.T, st store.PG, runnerID uuid.UUID, rows ...runnerwire.RunState) (store.RunnerInventoryResult, error) {
	t.Helper()
	session := uuid.NewString()
	if err := st.BeginRunnerInventory(t.Context(), runnerID, session); err != nil {
		return store.RunnerInventoryResult{}, err
	}
	rows = slices.Clone(rows)
	slices.SortFunc(rows, func(a, b runnerwire.RunState) int { return bytes.Compare(a.RunID[:], b.RunID[:]) })
	return st.IntakeRunnerStatePage(t.Context(), runnerID, session, runnerwire.StatePage{InventoryID: uuid.New(), Runs: rows, Final: true})
}

func TestPG_RunnerStateLapseAndContainment(t *testing.T) {
	pool, st, rn, run, _ := spoolPGFixture(t)
	ctx := t.Context()
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	gen := uuid.New()
	if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	stopProxies := func() int {
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM runner_pending_actions WHERE run_id=$1 AND kind='stop_proxy'`, run.ID).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	row := runnerwire.RunState{RunID: run.ID, SandboxPresent: true, Execution: &runnerwire.ExecutionResult{Generation: gen, Operation: runnerwire.MethodExec, Attempted: true}}
	if res, err := finalPage(t, st, rn.ID, row); err != nil || res.NeedsReconciliation {
		t.Fatalf("live reported run: %+v %v", res, err)
	}
	if changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, ref, time.Now()); err != nil || changed {
		t.Fatalf("fresh drop lost run: %v %v", changed, err)
	}
	if res, err := finalPage(t, st, rn.ID); err != nil || !res.NeedsReconciliation {
		t.Fatalf("omitted live run: %+v %v", res, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	if res, err := finalPage(t, st, rn.ID, row); err != nil || !res.NeedsReconciliation || stopProxies() != 0 {
		t.Fatalf("lapsed state: %+v %v stop_proxy=%d", res, err, stopProxies())
	}
	got, _ := st.GetRun(ctx, run.ID)
	if got.LostAt != nil {
		t.Fatal("checked STATE wrote lifecycle")
	}
	if changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, "stale-ref", time.Now()); changed || !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("stale reference: %v %v", changed, err)
	}
	if changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, ref, time.Now()); err != nil || !changed {
		t.Fatalf("loss claim: %v %v", changed, err)
	}
	if res, err := finalPage(t, st, rn.ID); err != nil || res.NeedsReconciliation {
		t.Fatalf("omitted kept run trapped reconnect: %+v %v", res, err)
	}
	if res, err := finalPage(t, st, rn.ID, row); err != nil || !res.NeedsReconciliation {
		t.Fatalf("late containment: %+v %v", res, err)
	}
	page, _, err := st.ListPendingRunnerActions(ctx, rn.ID)
	lost, _ := st.GetRun(ctx, run.ID)
	if err != nil || len(page) != 1 || page[0].Kind != types.RunnerActionStopProxy || page[0].Generation != gen || page[0].Ref != ref ||
		page[0].LostAt == nil || !page[0].LostAt.Equal(*lost.LostAt) {
		t.Fatalf("late containment queued %+v, %v", page, err)
	}
	observed := time.Now()
	row.ProxyStoppedAt = &observed
	if res, err := finalPage(t, st, rn.ID, row); err != nil || res.NeedsReconciliation {
		t.Fatalf("stopped proxy: %+v %v", res, err)
	}
	row.Execution.Generation = uuid.New()
	if _, err := finalPage(t, st, rn.ID, row); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("stale generation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=now() WHERE id=$1`, rn.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := finalPage(t, st, rn.ID); !errors.Is(err, store.ErrRunnerExecutionRefused) {
		t.Fatalf("revoked empty STATE: %v", err)
	}
}

func TestPG_RunnerLossClaimRechecksAuthority(t *testing.T) {
	for _, scenario := range []string{"renewed", "revoked", "foreign-owner"} {
		t.Run(scenario, func(t *testing.T) {
			pool, st, rn, run, _ := spoolPGFixture(t)
			ctx := t.Context()
			ref := "runner:" + rn.ID.String() + "/sandbox"
			if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
				t.Fatal(err)
			}
			if _, err := pool.Exec(ctx, `UPDATE agent_runs SET token_renewed_at=now()-interval '2 hours' WHERE id=$1`, run.ID); err != nil {
				t.Fatal(err)
			}
			candidates, err := st.ListRunnerLapsedRuns(ctx, rn.ID, uuid.Nil)
			if err != nil || len(candidates) != 1 {
				t.Fatalf("candidates: %d %v", len(candidates), err)
			}
			switch scenario {
			case "renewed":
				_, err = st.StampRunTokenRenewed(ctx, run.ID, "", "")
			case "revoked":
				_, err = pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=now() WHERE id=$1`, rn.ID)
			case "foreign-owner":
				_, err = pool.Exec(ctx, `UPDATE agent_runs SET created_by='another-owner' WHERE id=$1`, run.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			changed, err := st.MarkRunnerRunLost(ctx, rn.ID, run.ID, ref, time.Now())
			if changed || (scenario != "renewed" && !errors.Is(err, store.ErrRunnerExecutionRefused)) {
				t.Fatalf("authority race accepted: %v %v", changed, err)
			}
			got, _ := st.GetRun(ctx, run.ID)
			if got.LostAt != nil {
				t.Fatal("lost mark written")
			}
		})
	}
}

func TestPG_RunnerLateContainmentAtomicProof(t *testing.T) {
	t.Run("issued-execution", func(t *testing.T) { runnerLateContainmentProof(t, true) })
	t.Run("before-first-execution", func(t *testing.T) { runnerLateContainmentProof(t, false) })
}

func runnerLateContainmentProof(t *testing.T, issued bool) {
	pool, st, rn, run, _ := spoolPGFixture(t)
	ctx := t.Context()
	gen := uuid.New()
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(ctx, run.ID, ref); err != nil {
		t.Fatal(err)
	}
	if issued {
		if _, err := st.BeginRunnerExecution(ctx, rn.ID, run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
			t.Fatal(err)
		}
	} else {
		gen = uuid.Nil
	}
	if _, err := pool.Exec(ctx, `UPDATE agent_runs SET lost_at=now(),lost_reason='outage' WHERE id=$1`, run.ID); err != nil {
		t.Fatal(err)
	}
	got, err := st.GetRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	a, _, err := st.QueueRunnerAction(ctx, types.RunnerPendingAction{ID: uuid.New(), RunnerID: rn.ID, RunID: run.ID, Kind: types.RunnerActionStopProxy, Ref: ref, Generation: gen, LostAt: got.LostAt})
	if err != nil {
		t.Fatal(err)
	}
	observed := time.Now().Add(-time.Hour).Truncate(time.Microsecond)
	proof := store.RunnerContainmentProof{RunnerContainment: store.RunnerContainment{RunnerID: rn.ID, RunID: run.ID, Generation: gen, Ref: ref, LostAt: *got.LostAt}, ActionID: a.ID, ObservedAt: observed}
	apply := func(p store.RunnerContainmentProof, prepare func(context.Context, types.AuditEvent) (types.AuditEvent, error)) (bool, error) {
		tx, err := pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
		if err != nil {
			return false, err
		}
		defer tx.Rollback(ctx) //nolint:errcheck // closed after commit
		tag, err := tx.Exec(ctx, `UPDATE runner_pending_actions SET applied_at=clock_timestamp(),outcome='success',observed_at=$2 WHERE id=$1 AND applied_at IS NULL`, a.ID, observed)
		if err != nil {
			return false, err
		}
		if tag.RowsAffected() == 0 {
			return false, tx.Commit(ctx)
		}
		if err := st.RecordRunnerContainmentTx(ctx, tx, p, prepare); err != nil {
			return false, err
		}
		return true, tx.Commit(ctx)
	}
	for _, scenario := range []string{"old-lost-mark", "old-generation", "wrong-action", "seal-failure", "revoked"} {
		t.Run(scenario, func(t *testing.T) {
			p := proof
			prepare := prepareSpoolTest
			switch scenario {
			case "old-lost-mark":
				p.LostAt = p.LostAt.Add(-time.Second)
			case "old-generation":
				p.Generation = uuid.New()
			case "wrong-action":
				p.ActionID = uuid.New()
			case "revoked":
				if _, err := pool.Exec(ctx, `UPDATE runners SET state='revoked',revoked_at=now() WHERE id=$1`, rn.ID); err != nil {
					t.Fatal(err)
				}
				defer func() {
					if _, err := pool.Exec(ctx, `UPDATE runners SET state='claimed',revoked_at=NULL WHERE id=$1`, rn.ID); err != nil {
						t.Fatal(err)
					}
				}()

			case "seal-failure":
				prepare = func(context.Context, types.AuditEvent) (types.AuditEvent, error) {
					return types.AuditEvent{}, errors.New("seal unavailable")
				}
			}
			if changed, err := apply(p, prepare); err == nil || changed {
				t.Fatalf("invalid proof accepted: %v %v", changed, err)
			}
			var applied bool
			var count int
			if err := pool.QueryRow(ctx, `SELECT applied_at IS NOT NULL FROM runner_pending_actions WHERE id=$1`, a.ID).Scan(&applied); err != nil || applied {
				t.Fatalf("action CAS escaped rollback: %v %v", applied, err)
			}
			if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert'`, run.ID).Scan(&count); err != nil || count != 0 {
				t.Fatalf("audit escaped rollback: %d %v", count, err)
			}
		})
	}
	if changed, err := apply(proof, prepareSpoolTest); err != nil || !changed {
		t.Fatalf("exact proof: %v %v", changed, err)
	}
	if changed, err := apply(proof, prepareSpoolTest); err != nil || changed {
		t.Fatalf("duplicate action: %v %v", changed, err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events WHERE run_id=$1 AND action='run.containment.reassert' AND time>$2`, run.ID, observed).Scan(&count); err != nil || count != 1 {
		t.Fatalf("arrival-clock audit: %d %v", count, err)
	}
	final, _ := st.GetRun(ctx, run.ID)
	if final.State != types.RunRunning || final.LostAt == nil || !final.LostAt.Equal(*got.LostAt) {
		t.Fatal("late proof restored lifecycle")
	}
}

// goneRig is spoolPGFixture with the run's sandbox ref set, plus the rows and clock moves the gone
// tests share: gone is the runner's report of the sandbox absent with no exit code, present its twin.
type goneRig struct {
	t             *testing.T
	pool          *pgxpool.Pool
	st            store.PG
	rn            types.Runner
	run           types.AgentRun
	ref           string
	gone, present runnerwire.RunState
}

func newGoneRig(t *testing.T) *goneRig {
	t.Helper()
	pool, st, rn, run, _ := spoolPGFixture(t)
	ref := "runner:" + rn.ID.String() + "/sandbox"
	if err := st.SetSandboxRef(t.Context(), run.ID, ref); err != nil {
		t.Fatal(err)
	}
	gone := runnerwire.RunState{RunID: run.ID, Ref: ref}
	present := gone
	present.SandboxPresent = true
	return &goneRig{t: t, pool: pool, st: st, rn: rn, run: run, ref: ref, gone: gone, present: present}
}

func (g *goneRig) exec(q string, args ...any) {
	g.t.Helper()
	if _, err := g.pool.Exec(g.t.Context(), q, args...); err != nil {
		g.t.Fatal(err)
	}
}

func (g *goneRig) page(rows ...runnerwire.RunState) store.RunnerInventoryResult {
	g.t.Helper()
	res, err := finalPage(g.t, g.st, g.rn.ID, rows...)
	if err != nil {
		g.t.Fatal(err)
	}
	return res
}

func (g *goneRig) stamp() *time.Time {
	g.t.Helper()
	var at *time.Time
	if err := g.pool.QueryRow(g.t.Context(), `SELECT sandbox_absent_at FROM agent_runs WHERE id=$1`, g.run.ID).Scan(&at); err != nil {
		g.t.Fatal(err)
	}
	return at
}

// ageStamp moves the first absent report 31 s back; ageBoth moves the current inventory with it.
func (g *goneRig) ageStamp() {
	g.t.Helper()
	g.exec(`UPDATE agent_runs SET sandbox_absent_at=sandbox_absent_at-interval '31 seconds' WHERE id=$1`, g.run.ID)
}

func (g *goneRig) ageBoth() {
	g.t.Helper()
	g.ageStamp()
	g.exec(`UPDATE runner_inventories SET created_at=created_at-interval '31 seconds' WHERE runner_id=$1`, g.rn.ID)
}

func (g *goneRig) claim() (bool, error) {
	return g.st.MarkRunnerRunGone(g.t.Context(), g.rn.ID, g.run.ID, g.ref, time.Now())
}

// confirm sends two absent reports from fresh sessions more than RunnerSandboxGoneAfter apart.
func (g *goneRig) confirm(row runnerwire.RunState) store.RunnerInventoryResult {
	g.t.Helper()
	g.page(row)
	g.ageStamp()
	return g.page(row)
}

func (g *goneRig) holders() (slots, macs int) {
	g.t.Helper()
	ids := []uuid.UUID{g.rn.ID}
	s, err := g.st.RunnerSlotHolders(g.t.Context(), ids)
	if err != nil {
		g.t.Fatal(err)
	}
	m, err := g.st.RunnerMacOSVMHolders(g.t.Context(), ids)
	if err != nil {
		g.t.Fatal(err)
	}
	return s[g.rn.ID], m[g.rn.ID]
}

func TestPG_RunnerSandboxGoneNeedsTwoSessionsThirtySecondsApart(t *testing.T) {
	g := newGoneRig(t)
	ctx := t.Context()
	if res := g.page(g.gone); !res.NeedsReconciliation || res.GoneDue || g.stamp() == nil {
		t.Fatalf("first absent report: %+v stamp=%v", res, g.stamp())
	}
	g.ageBoth()
	if ok, err := g.claim(); ok || err != nil {
		t.Fatalf("one report confirmed gone: %v %v", ok, err)
	}
	if runs, err := g.st.ListRunnerGoneRuns(ctx, g.rn.ID, uuid.Nil); err != nil || len(runs) != 0 {
		t.Fatalf("one report listed gone: %d %v", len(runs), err)
	}
	if g.page(g.present); g.stamp() != nil {
		t.Fatal("a present report kept the absent stamp")
	}
	g.page(g.gone)
	if res := g.page(g.gone); res.GoneDue {
		t.Fatalf("two reports at once confirmed gone: %+v", res)
	}
	if ok, err := g.claim(); ok || err != nil {
		t.Fatalf("two reports at once claimed: %v %v", ok, err)
	}
	g.exec(`UPDATE agent_runs SET machine_kind='macos_vm' WHERE id=$1`, g.run.ID)
	g.ageStamp()
	if res := g.page(g.gone); !res.GoneDue {
		t.Fatalf("reports 31 s apart not due: %+v", res)
	}
	if runs, err := g.st.ListRunnerGoneRuns(ctx, g.rn.ID, uuid.Nil); err != nil || len(runs) != 1 || runs[0].ID != g.run.ID {
		t.Fatalf("confirmed run not listed: %v %v", runs, err)
	}
	if slots, macs := g.holders(); slots != 1 || macs != 1 {
		t.Fatalf("holders before the mark: slots=%d macs=%d", slots, macs)
	}
	if ok, err := g.claim(); !ok || err != nil {
		t.Fatalf("confirmed claim: %v %v", ok, err)
	}
	got, err := g.st.GetRun(ctx, g.run.ID)
	if err != nil || got.State != types.RunRunning || got.LostReason != types.LostGone || got.LostAt == nil || got.PausedAt != nil {
		t.Fatalf("marked run: %+v %v", got, err)
	}
	if slots, macs := g.holders(); slots != 0 || macs != 0 {
		t.Fatalf("a gone run still holds: slots=%d macs=%d", slots, macs)
	}
	if ok, err := g.claim(); ok || err != nil {
		t.Fatalf("second claim: %v %v", ok, err)
	}
}

func TestPG_RunnerSandboxGoneGuards(t *testing.T) {
	cases := map[string]func(g *goneRig) runnerwire.RunState{
		// Characterization: the row this report writes also blocks it (goneClaimableSQL's NOT EXISTS, store_runner_state.go:104).
		"containment_pending_report": func(g *goneRig) runnerwire.RunState {
			row := g.gone
			row.ContainmentPending = &runnerwire.ContainmentPending{Kind: "agent_only", Since: time.Now()}
			return row
		},
		"containment_pending_row": func(g *goneRig) runnerwire.RunState {
			g.exec(`INSERT INTO run_containment_pending (run_id, runner_id, kind, since) VALUES ($1, $2, 'agent_only', now())`, g.run.ID, g.rn.ID)
			return g.gone
		},
		"unfinished_staged_exit": func(g *goneRig) runnerwire.RunState {
			g.exec(`INSERT INTO runner_execution_exits (run_id, generation, runner_id, payload, payload_hash) VALUES ($1, $2, $3, '{}', 'h')`, g.run.ID, uuid.New(), g.rn.ID)
			return g.gone
		},
		"other_ref": func(g *goneRig) runnerwire.RunState {
			row := g.gone
			row.Ref = "runner:" + g.rn.ID.String() + "/other"
			return row
		},
		"retirement": func(g *goneRig) runnerwire.RunState {
			row := g.gone
			row.Retirement = &types.RunnerRetirement{}
			return row
		},
		"outage_kept_agent_stopped": func(g *goneRig) runnerwire.RunState {
			g.exec(`UPDATE agent_runs SET lost_at=now()-interval '2 hours', lost_reason='outage', agent_stopped_at=now() WHERE id=$1`, g.run.ID)
			return g.gone
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			g := newGoneRig(t)
			g.confirm(setup(g))
			if at := g.stamp(); at != nil {
				t.Fatalf("stamped under a guard: %v", at)
			}
			if ok, err := g.claim(); ok || err != nil {
				t.Fatalf("claimed under a guard: %v %v", ok, err)
			}
		})
	}
	t.Run("outage_kept_agent_running", func(t *testing.T) {
		g := newGoneRig(t)
		g.exec(`UPDATE agent_runs SET lost_at=now()-interval '2 hours', lost_reason='outage' WHERE id=$1`, g.run.ID)
		g.confirm(g.gone)
		if ok, err := g.claim(); !ok || err != nil {
			t.Fatalf("outage-kept run holding its sandbox not claimed: %v %v", ok, err)
		}
		if got, err := g.st.GetRun(t.Context(), g.run.ID); err != nil || got.LostReason != types.LostGone {
			t.Fatalf("outage-kept claim: %+v %v", got, err)
		}
	})
}

func TestPG_RunnerSandboxGoneAuthority(t *testing.T) {
	g := newGoneRig(t)
	ctx := t.Context()
	g.confirm(g.gone)
	for name, c := range map[string]struct {
		runner uuid.UUID
		ref    string
	}{"another_runner": {uuid.New(), g.ref}, "empty_ref": {g.rn.ID, ""}, "stale_ref": {g.rn.ID, g.ref + "-stale"}} {
		if ok, err := g.st.MarkRunnerRunGone(ctx, c.runner, g.run.ID, c.ref, time.Now()); ok || !errors.Is(err, store.ErrRunnerExecutionRefused) {
			t.Errorf("%s: %v %v", name, ok, err)
		}
	}
	if ok, err := g.claim(); !ok || err != nil {
		t.Fatalf("authorised claim: %v %v", ok, err)
	}
}

// Characterization: main's terminal arm and DR-0436's arm B both keep the exit receipt_only (store_runner_exit_stage.go:72-77, :88-94).
func TestPG_RunnerSandboxGoneThenExitIsReceipt(t *testing.T) {
	g := newGoneRig(t)
	ctx := t.Context()
	gen := uuid.New()
	if _, err := g.st.BeginRunnerExecution(ctx, g.rn.ID, g.run.ID, uuid.Nil, gen, runnerwire.MethodExec); err != nil {
		t.Fatal(err)
	}
	g.confirm(g.gone)
	if ok, err := g.claim(); !ok || err != nil {
		t.Fatalf("claim: %v %v", ok, err)
	}
	got, err := g.st.GetRun(ctx, g.run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := g.st.StopKeptRunIf(ctx, g.run.ID, types.RunFailed, got.LostAt, types.LostGone, got.EndsAt); !ok || err != nil {
		t.Fatalf("end gone run: %v %v", ok, err)
	}
	exit := runnerwire.AgentExit{Generation: gen, ExecID: "exec-gone", Ref: g.ref, Code: 0, ObservedAt: time.Now()}
	if err := g.st.StageRunnerExit(ctx, g.rn.ID, g.run.ID, exit); err != nil {
		t.Fatalf("late exit refused: %v", err)
	}
	var receipt bool
	if err := g.pool.QueryRow(ctx, `SELECT receipt_only FROM runner_execution_exits WHERE run_id=$1 AND generation=$2`, g.run.ID, gen).Scan(&receipt); err != nil || !receipt {
		t.Fatalf("late exit receipt_only=%v %v", receipt, err)
	}
}
