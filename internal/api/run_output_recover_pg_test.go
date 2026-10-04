// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Recovering a run's output from the substrate against a real Postgres: a second
// Server over the same database is a wardynd that restarted or a replica that
// adopted the run, with its own registry and manifest cache, so whether it may
// read the substrate at all rests on the manifest row. Guarded by WARDYN_TEST_PG
// (throwawayPGPool); skipped cleanly when unset.

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// owedPending is the pending row the dispatching process left, claimed long ago.
func (l *maskLab) owedPending(runID uuid.UUID) {
	l.t.Helper()
	if _, err := l.pool.Exec(context.Background(),
		`INSERT INTO run_outputs (run_id, source, claimed_at) VALUES ($1, 'stdout', now() - interval '1 hour')`, runID); err != nil {
		l.t.Fatal(err)
	}
}

// storedRow reads the final row's flags straight from the columns.
func (l *maskLab) storedRow(runID uuid.UUID) (output string, gap, incomplete bool, scope *string) {
	l.t.Helper()
	if err := l.pool.QueryRow(context.Background(),
		`SELECT convert_from(output, 'UTF8'), capture_gap, incomplete, mask_scope FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, runID).
		Scan(&output, &gap, &incomplete, &scope); err != nil {
		l.t.Fatalf("read the final row of %s: %v", runID, err)
	}
	return output, gap, incomplete, scope
}

// adoptedOn is a replica that restarted: no tail, no registry values, over a
// substrate that can give the run's log back.
func (l *maskLab) adoptedOn(rr *recoveringRunner) replica {
	l.t.Helper()
	b := l.replica()
	b.srv.cfg.Runner = rr
	return b
}

// A run with no complete manifest (one from before 0.8.6, or one whose dispatch
// never completed it) is never read from the substrate: this process cannot say
// whether its registry holds the run's secrets, so the row is a capture gap with
// no bytes, whether the run is finalised after the restart or adopted live.
func TestRecoverRunOutputPG_NoCompleteManifestMakesNoSubstrateRead(t *testing.T) {
	for name, setup := range map[string]func(replica, types.AgentRun){
		"no manifest at all": func(replica, types.AgentRun) {},
		"an incomplete one": func(a replica, run types.AgentRun) {
			if !a.srv.beginMaskManifest(t.Context(), run) {
				t.Fatal("the manifest could not begin")
			}
		},
	} {
		for how, end := range map[string]func(*testing.T, *maskLab, replica, types.AgentRun, *recoveringRunner){
			"finalised": func(t *testing.T, _ *maskLab, b replica, run types.AgentRun, _ *recoveringRunner) {
				b.srv.reconcileFinalize(t.Context(), run.ID, types.RunCompleted, "sbx-1", "reconciled exit")
			},
			"adopted live": func(t *testing.T, lab *maskLab, b replica, run types.AgentRun, rr *recoveringRunner) {
				prev := reconcileWatchIntervalNS.Swap(int64(20 * time.Millisecond))
				t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
				go b.srv.reconcileWatch(b.srv.cfg.BaseCtx, run.ID, "sbx-1", "exec-1")
				time.Sleep(100 * time.Millisecond) // the resume would have read by now
				close(rr.exited)
				waitFor(t, "the run to end", func() bool {
					return lab.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, run.ID) == 1
				})
			},
		} {
			t.Run(name+"/"+how, func(t *testing.T) {
				lab := newMaskLab(t)
				a := lab.replica()
				run := lab.run()
				setup(a, run)
				lab.owedPending(run.ID)
				rr := newRecoveringRunner()
				rr.log = "a dispatch-time secret printed whole\n"
				b := lab.adoptedOn(rr)
				end(t, lab, b, run, rr)
				waitFor(t, "the final row", func() bool {
					return lab.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, run.ID) == 1
				})
				b.srv.WaitBackground()
				out, gap, incomplete, scope := lab.storedRow(run.ID)
				if !gap || out != "" || incomplete || scope != nil {
					t.Fatalf("row %q gap=%v incomplete=%v scope=%v, want a capture gap with no bytes", out, gap, incomplete, scope)
				}
				if rr.recoverCount() != 0 || rr.execCount() != 0 {
					t.Fatalf("%d substrate reads and %d execs, want none: the coverage check comes before the read", rr.recoverCount(), rr.execCount())
				}
			})
		}
	}
}

// A run whose manifest is complete is recovered on a replica that never saw its
// values, masked against the manifest: a secret printed whole, and one split
// across the handoff, is absent from run_outputs.output read with SQL.
func TestRecoverRunOutputPG_CoveredRunIsRecoveredMasked(t *testing.T) {
	const secret = "adopted-secret-value-9"
	for _, how := range []string{"finalised", "adopted live"} {
		t.Run(how, func(t *testing.T) {
			lab := newMaskLab(t)
			a := lab.replica()
			run := lab.run()
			a.dispatch(t, run, secret)
			lab.owedPending(run.ID)

			rr := newRecoveringRunner()
			rr.log = "whole " + secret + " here\nsplit adopted-secret-"
			b := lab.adoptedOn(rr)
			if how == "finalised" {
				b.srv.reconcileFinalize(t.Context(), run.ID, types.RunCompleted, "sbx-1", "reconciled exit")
			} else {
				prev := reconcileWatchIntervalNS.Swap(int64(20 * time.Millisecond))
				t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
				rr.follow, rr.tail = make(chan struct{}), "value-9 there\n"
				go b.srv.reconcileWatch(b.srv.cfg.BaseCtx, run.ID, "sbx-1", "exec-1")
				waitFor(t, "B to resume the log", func() bool { return b.srv.tailFor(run.ID) != nil && rr.recoverCount() == 1 })
				close(rr.follow)
				close(rr.exited)
			}
			waitFor(t, "the final row", func() bool {
				return lab.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, run.ID) == 1
			})
			b.srv.WaitBackground()

			out, gap, incomplete, scope := lab.storedRow(run.ID)
			if strings.Contains(out, "adopted-secret-") || gap || incomplete || scope == nil || *scope != "run" {
				t.Fatalf("row %q gap=%v incomplete=%v scope=%v: the secret must never reach the column", out, gap, incomplete, scope)
			}
			want := "whole <secret-hidden> here\nsplit "
			if how == "adopted live" {
				want += "<secret-hidden> there\n"
			} else {
				want += "<secret-hidden>" // the cut-short prefix is hidden at the end of the capture
			}
			if out != want {
				t.Fatalf("run_outputs.output = %q, want %q", out, want)
			}
			if rr.recoverCount() != 1 || rr.execCount() != 0 {
				t.Fatalf("%d reads and %d execs, want one read and no re-run", rr.recoverCount(), rr.execCount())
			}
		})
	}
}

// Coverage lost in the middle of a recovery ends it as a capture gap, never as a
// row masked by the globals alone.
func TestRecoverRunOutputPG_CoverageLostMidReadIsACaptureGap(t *testing.T) {
	lab := newMaskLab(t)
	a, b0 := lab.replica(), lab.replica()
	run := lab.run()
	a.dispatch(t, run, "fenced-mid-read-value")
	lab.owedPending(run.ID)

	rr := newRecoveringRunner()
	rr.log, rr.follow, rr.tail = "covered so far\n", make(chan struct{}), "printed after the fence fenced-mid-read-value\n"
	b := lab.adoptedOn(rr)
	prev := reconcileWatchIntervalNS.Swap(int64(20 * time.Millisecond))
	t.Cleanup(func() { reconcileWatchIntervalNS.Store(prev) })
	go b.srv.reconcileWatch(b.srv.cfg.BaseCtx, run.ID, "sbx-1", "exec-1")
	waitFor(t, "B to resume the log", func() bool { return b.srv.tailFor(run.ID) != nil && rr.recoverCount() == 1 })

	if fenced, err := b0.srv.cfg.MaskManifests.FenceSubject(t.Context(), maskOwner); err != nil || len(fenced) != 1 {
		t.Fatalf("FenceSubject = %v, %v", fenced, err)
	}
	waitFor(t, "B's writer to see the fence", func() bool {
		_, _ = b.srv.tailFor(run.ID).mw.Write([]byte("."))
		e := b.srv.tailFor(run.ID)
		e.mw.mu.Lock()
		defer e.mw.mu.Unlock()
		return e.mw.capture.uncovered
	})
	close(rr.follow)
	close(rr.exited)
	waitFor(t, "the final row", func() bool {
		return lab.count(`SELECT count(*) FROM run_outputs WHERE run_id=$1 AND captured_at IS NOT NULL`, run.ID) == 1
	})
	b.srv.WaitBackground()
	out, gap, _, scope := lab.storedRow(run.ID)
	if !gap || out != "" || scope != nil {
		t.Fatalf("row %q gap=%v scope=%v, want a capture gap with no bytes and no globals_only scope", out, gap, scope)
	}
	if n := lab.count(`SELECT count(*) FROM run_outputs WHERE mask_scope = 'globals_only'`); n != 0 {
		t.Fatalf("%d globals_only rows from a recovery, want none", n)
	}
}

// The retention sweeper resolves a terminal run's abandoned pending row by
// recovery when its manifest covers it and the sandbox still answers, and to a
// capture gap, with no read, when it has no complete manifest.
func TestRecoverRunOutputPG_SweeperRecoversCoveredRunsOnly(t *testing.T) {
	const secret = "swept-secret-value-77"
	lab := newMaskLab(t)
	a := lab.replica()
	covered, bare := lab.run(), lab.run()
	a.dispatch(t, covered, secret)
	for _, r := range []types.AgentRun{covered, bare} {
		if _, err := lab.pool.Exec(t.Context(), `UPDATE agent_runs SET state='FAILED' WHERE id=$1`, r.ID); err != nil {
			t.Fatal(err)
		}
		lab.owedPending(r.ID)
	}
	rr := newRecoveringRunner()
	rr.log = "swept " + secret + " ok\n"
	b := lab.adoptedOn(rr)
	if err := b.srv.SweepRunOutputs(t.Context()); err != nil {
		t.Fatal(err)
	}
	out, gap, incomplete, scope := lab.storedRow(covered.ID)
	if out != "swept <secret-hidden> ok\n" || gap || incomplete || scope == nil || *scope != "run" {
		t.Fatalf("covered run: row %q gap=%v incomplete=%v scope=%v, want it recovered and masked", out, gap, incomplete, scope)
	}
	if out, gap, _, _ := lab.storedRow(bare.ID); !gap || out != "" {
		t.Fatalf("manifest-less run: row %q gap=%v, want a capture gap", out, gap)
	}
	if rr.recoverCount() != 1 {
		t.Fatalf("%d substrate reads, want one (the covered run's)", rr.recoverCount())
	}
}
