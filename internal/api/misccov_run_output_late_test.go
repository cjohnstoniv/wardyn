// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A byte that arrives after the seal and before the row commits is dropped and marks the row
// incomplete, whether the commit succeeds first time or after a retry.
func TestMiscCovLateByteBeforeTheCommitMarksTheRowIncomplete(t *testing.T) {
	t.Run("the first write commits and the row is marked afterwards", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		w := f.open(t)
		writeExecOutput(t, w, "early\n")
		cs.saveFinalFn = func(call int) error {
			if call == 1 {
				_, _ = w.Write([]byte("late\n"))
			}
			return nil
		}
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		f.srv.WaitBackground()
		row := f.finalRow(t)
		if string(row.Output) != "early\n" {
			t.Errorf("row holds %q, want the early bytes only", row.Output)
		}
		if got, _ := f.mem.row(f.run.ID); !got.Incomplete {
			t.Error("the committed row was not marked incomplete by the late byte")
		}
		got := miscCovFinalizeData(t, f)
		if len(got) != 1 || got[0]["late_write"] != true || got[0]["incomplete"] != true {
			t.Errorf("audit = %v, want one row flagged late_write and incomplete", got)
		}
	})
	t.Run("a failed write is retried and the row is written incomplete", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		w := f.open(t)
		writeExecOutput(t, w, "early\n")
		cs.saveFinalFn = func(call int) error {
			if call == 1 {
				_, _ = w.Write([]byte("late\n"))
				return errors.New("postgres is down")
			}
			return nil
		}
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		row := f.finalRow(t)
		if string(row.Output) != "early\n" || !row.Incomplete || cs.saveFinalCalls != 2 {
			t.Fatalf("row %q incomplete=%v after %d writes, want the early bytes, incomplete, after two", row.Output, row.Incomplete, cs.saveFinalCalls)
		}
		got := miscCovFinalizeData(t, f)
		if len(got) != 1 || got[0]["incomplete"] != true || got[0]["late_write"] != nil {
			t.Errorf("audit = %v, want one incomplete row written as such, not a later mark", got)
		}
	})
}

func TestMiscCovMarkLateIncomplete(t *testing.T) {
	t.Run("no run-output store has nothing to mark", func(t *testing.T) {
		f, _ := miscCovOutFixture(t, func(c *Config) { c.RunOutputPersistOff = true })
		now := time.Now()
		f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, CapturedAt: &now}
		f.srv.markLateIncomplete(t.Context(), f.run.ID)
		if row, _ := f.mem.row(f.run.ID); row.Incomplete || len(miscCovFinalizeData(t, f)) != 0 {
			t.Errorf("with persistence off a committed row was marked (%+v) or audited", row)
		}
	})
	t.Run("a run with no final row is not audited", func(t *testing.T) {
		f, _ := miscCovOutFixture(t)
		f.srv.markLateIncomplete(t.Context(), f.run.ID)
		if len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("a row that was never marked was audited")
		}
	})
	t.Run("an erased run fences the tail", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		f.open(t)
		cs.markErr = store.ErrRunOutputErased
		f.srv.markLateIncomplete(t.Context(), f.run.ID)
		if f.srv.tailFor(f.run.ID) != nil || len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("the tail was not fenced, or an erased run was audited")
		}
	})
	t.Run("a failed mark is logged and not audited", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.markErr = errors.New("mark refused")
		logs := miscCovCaptureLogs(t)
		f.srv.markLateIncomplete(t.Context(), f.run.ID)
		if _, ok := logs.find("could not mark a run's output row incomplete"); !ok || len(miscCovFinalizeData(t, f)) != 0 {
			t.Error("the failure was not logged, or a mark that failed was audited")
		}
	})
}

// A final write that keeps failing past the inline attempts is audited as persist_failing and carried on
// by a tracked retry, which a cancelled request does not shorten and a shutting-down daemon ends.
func TestMiscCovFinalWriteRetryOutlivesTheRequestAndEndsWithTheDaemon(t *testing.T) {
	t.Run("a request that is gone still gets its row from the retry", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.saveFinalFn = func(call int) error {
			if call <= runOutputSyncAttempts {
				return errors.New("postgres is down")
			}
			return nil
		}
		writeExecOutput(t, f.open(t), "output\n")
		gone, cancel := context.WithCancel(t.Context())
		cancel()
		f.srv.FinishRunOutput(gone, f.run.ID)
		f.srv.WaitBackground()
		row := f.finalRow(t)
		if string(row.Output) != "output\n" || cs.saveFinalCalls != runOutputSyncAttempts+1 {
			t.Fatalf("row %q after %d writes, want the output after %d", row.Output, cs.saveFinalCalls, runOutputSyncAttempts+1)
		}
		if got := miscCovFinalizeData(t, f); len(got) != 1 || got[0]["persist_failing"] != true {
			t.Errorf("audit = %v, want one persist_failing row", got)
		}
	})
	t.Run("a daemon that is shutting down leaves the pending row to the sweeper", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		f, cs := miscCovOutFixture(t, func(c *Config) { c.BaseCtx = ctx })
		cs.saveFinalFn = func(int) error { return errors.New("postgres is down") }
		writeExecOutput(t, f.open(t), "output\n")
		f.srv.FinishRunOutput(t.Context(), f.run.ID)
		f.srv.WaitBackground()
		if cs.saveFinalCalls != runOutputSyncAttempts {
			t.Errorf("%d writes, want only the %d inline attempts", cs.saveFinalCalls, runOutputSyncAttempts)
		}
		if row, ok := f.mem.row(f.run.ID); !ok || row.CapturedAt != nil {
			t.Errorf("row = %+v (found %v), want the pending row left as it was", row, ok)
		}
	})
}

func TestMiscCovSealStopsTheChunkMirror(t *testing.T) {
	f, _ := miscCovOutFixture(t)
	w := f.open(t)
	q := miscCovQueue(t, f)
	writeExecOutput(t, w, "output\n")
	f.srv.FinishRunOutput(t.Context(), f.run.ID)
	f.srv.WaitBackground()
	q.mu.Lock()
	stopped, pend := q.stopped, len(q.pend)
	q.mu.Unlock()
	if !stopped || pend != 0 {
		t.Errorf("queue stopped %v with %d bytes after the seal, want a stopped, empty queue", stopped, pend)
	}
	if string(f.finalRow(t).Output) != "output\n" {
		t.Error("the final row does not hold the output")
	}
}

func TestMiscCovPrepareRunOutputLeavesARunItCannotReadAlone(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	cs.getRunErr = errors.New("run unreadable")
	f.srv.prepareRunOutput(t.Context(), f.run.ID, false)
	if _, ok := f.mem.row(f.run.ID); ok {
		t.Error("a row was written for a run that could not be read")
	}
	if f.rn.readCount() != 0 {
		t.Errorf("the substrate was read %d times for a run that could not be read", f.rn.readCount())
	}
}

func TestMiscCovEraseRunOutputsKeepsTheTailsWhenTheStoreFails(t *testing.T) {
	f, cs := miscCovOutFixture(t)
	f.open(t)
	boom := errors.New("erase refused")
	cs.eraseErr = boom
	if err := f.srv.EraseRunOutputs(t.Context(), []uuid.UUID{f.run.ID}); !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the store's error", err)
	}
	if f.srv.tailFor(f.run.ID) == nil {
		t.Error("the tail was fenced although nothing was erased: the memory copy was dropped for nothing")
	}
}

func TestMiscCovSweepRunOutputs(t *testing.T) {
	retention := 72 * time.Hour

	t.Run("a store with no run outputs has nothing to sweep", func(t *testing.T) {
		if err := newHarness(t).srv.SweepRunOutputs(t.Context()); err != nil {
			t.Errorf("err = %v, want nil", err)
		}
	})
	t.Run("rows deleted past retention are audited with the window", func(t *testing.T) {
		f, cs := miscCovOutFixture(t, func(c *Config) { c.RunOutputRetention = retention })
		cs.deleteN = 3
		if err := f.srv.SweepRunOutputs(t.Context()); err != nil {
			t.Fatal(err)
		}
		f.audit.mu.Lock()
		defer f.audit.mu.Unlock()
		var rows []types.AuditEvent
		for _, ev := range f.audit.events {
			if ev.Action == "run.output.retention.sweep" {
				rows = append(rows, ev)
			}
		}
		if len(rows) != 1 || rows[0].Outcome != "success" || !bytes.Contains(rows[0].Data, []byte(`"deleted":3`)) || !bytes.Contains(rows[0].Data, []byte(`"retention_days":3`)) {
			t.Errorf("sweep rows = %+v, want one success row with deleted 3 and retention_days 3", rows)
		}
	})
	t.Run("a failed delete is returned after the stale pass still ran", func(t *testing.T) {
		f, cs := miscCovOutFixture(t, func(c *Config) { c.RunOutputRetention = retention })
		cs.deleteErr = errors.New("delete refused")
		if err := f.srv.SweepRunOutputs(t.Context()); !errors.Is(err, cs.deleteErr) {
			t.Errorf("err = %v, want the delete's error", err)
		}
		if cs.listed != 1 {
			t.Errorf("stale rows listed %d times, want the pass to go on", cs.listed)
		}
	})
	t.Run("with persistence off there are no pending rows to resolve", func(t *testing.T) {
		f, cs := miscCovOutFixture(t, func(c *Config) { c.RunOutputRetention = retention; c.RunOutputPersistOff = true })
		cs.deleteErr = errors.New("delete refused")
		if err := f.srv.SweepRunOutputs(t.Context()); !errors.Is(err, cs.deleteErr) {
			t.Errorf("err = %v, want the delete's error", err)
		}
		if cs.listed != 0 {
			t.Errorf("stale rows listed %d times with persistence off, want none", cs.listed)
		}
	})
	t.Run("a failed list is the pass's error", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.listErr = errors.New("list refused")
		if err := f.srv.SweepRunOutputs(t.Context()); !errors.Is(err, cs.listErr) {
			t.Errorf("err = %v, want the list's error", err)
		}
	})
}

// A stale pending row of a run that cannot be read, or whose terminal is an interactive session, becomes
// a capture gap rather than a recovery.
func TestMiscCovResolveStalePendingWritesAGapWhenThereIsNothingToRecover(t *testing.T) {
	t.Run("a run that cannot be read", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		cs.getRunErr = errors.New("run unreadable")
		f.srv.resolveStalePending(t.Context(), cs, f.run.ID)
		if r, ok := f.mem.row(f.run.ID); !ok || !r.CaptureGap {
			t.Errorf("row = %+v (found %v), want a capture gap", r, ok)
		}
		if got := miscCovFinalizeData(t, f); len(got) != 1 || got[0]["reason"] != "stale_pending" {
			t.Errorf("audit = %v, want one gap row with reason stale_pending", got)
		}
	})
	t.Run("an interactive run", func(t *testing.T) {
		f, cs := miscCovOutFixture(t)
		f.st.run.Interactive = true
		f.srv.resolveStalePending(t.Context(), cs, f.run.ID)
		if r, ok := f.mem.row(f.run.ID); !ok || !r.CaptureGap {
			t.Errorf("row = %+v (found %v), want a capture gap", r, ok)
		}
		if f.rn.readCount() != 0 {
			t.Errorf("the substrate was read %d times for an interactive run", f.rn.readCount())
		}
	})
}
