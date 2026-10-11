// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

func uncoveredOutputFixture(t *testing.T) (*outputFixture, *gapCovOutStore) {
	t.Helper()
	var gs *gapCovOutStore
	f := newOutputFixture(t, func(c *Config) {
		gs = &gapCovOutStore{miscCovOutStore: newMiscCovOutStore(c.Store.(*memRunOutputs))}
		c.Store = gs
		pool, _ := miscCovClosedPool(t)
		c.MaskRegistry = secretmask.NewRegistry()
		c.MaskManifests = maskmanifest.New(pool, nil, c.MaskRegistry)
	})
	return f, gs
}

func execRelayDenials(f *outputFixture) int {
	n := 0
	for _, ev := range f.audit.eventsFor(f.run.ID, authz.AuditAction) {
		if ev.Target == "exec.relay" && ev.Outcome == "denied" && bytes.Contains(ev.Data, []byte(string(authz.ReasonMaskStateUnavailable))) {
			n++
		}
	}
	return n
}

// Door 3 keeps nothing for a run whose masking it cannot prove, and says so:
// a denied row, a warning, and the pending row the sweeper turns into a capture
// gap with its reason when this process never finalises the run.
func TestOpenExecOutput_RefusalIsAuditedAndLeavesAPendingRowForTheSweeper(t *testing.T) {
	f, gs := uncoveredOutputFixture(t)
	logs := miscCovCaptureLogs(t)

	if w := f.srv.openExecOutput(f.run, false); w != nil {
		t.Fatal("an uncovered run got an exec output writer")
	}
	if n := execRelayDenials(f); n != 1 {
		t.Errorf("exec.relay denied rows = %d, want 1", n)
	}
	if _, ok := logs.find("exec output relay is refused"); !ok {
		t.Error("the refusal was not logged")
	}
	if r, ok := f.mem.row(f.run.ID); !ok || r.CapturedAt != nil || r.CaptureGap {
		t.Fatalf("row = %+v (found %v), want a pending row", r, ok)
	}

	f.srv.resolveStalePending(t.Context(), gs, f.run.ID)

	if r, _ := f.mem.row(f.run.ID); !r.CaptureGap || len(r.Output) != 0 {
		t.Errorf("row = %+v, want an empty capture gap", r)
	}
	if got := f.gapReason(t); got != "mask_uncovered" {
		t.Errorf("gap reason = %q, want mask_uncovered", got)
	}
}

// A pending row that cannot be written does not lift the refusal.
func TestOpenExecOutput_RefusalHoldsWhenThePendingRowCannotBeWritten(t *testing.T) {
	f, gs := uncoveredOutputFixture(t)
	gs.insertErr = errors.New("output store down")
	logs := miscCovCaptureLogs(t)

	if w := f.srv.openExecOutput(f.run, false); w != nil {
		t.Fatal("an uncovered run got an exec output writer")
	}
	if n := execRelayDenials(f); n != 1 {
		t.Errorf("exec.relay denied rows = %d, want 1", n)
	}
	if _, ok := logs.find("could not record that a run's output capture is owed"); !ok {
		t.Error("the failed pending insert was not logged")
	}
	if _, ok := f.mem.row(f.run.ID); ok {
		t.Error("a row appeared although the insert failed")
	}
}

// An erased run's refusal fences the run's output and writes no pending row.
func TestOpenExecOutput_RefusalOfAnErasedRunKeepsNothing(t *testing.T) {
	f, _ := uncoveredOutputFixture(t)
	f.mem.erased[f.run.ID] = true

	if w := f.srv.openExecOutput(f.run, false); w != nil {
		t.Fatal("an erased run got an exec output writer")
	}
	if _, ok := f.mem.row(f.run.ID); ok {
		t.Error("an erased run got a pending row")
	}
}

// A run that failed while STARTING, before dispatch opened any output, owes
// none: finalising it writes no gap row and no finalize audit. A run that
// opened output (a pending row), or one with a sandbox and no row (from before
// the row existed), still owes the capture.
func TestFinishRunOutput_NeverStartedRunOwesNothing(t *testing.T) {
	cases := []struct {
		name    string
		ref     string
		pending bool
		want    bool
	}{
		{"never started", "", false, false},
		{"output was opened, then the run failed before a sandbox ref", "", true, true},
		{"a sandbox and no row", "sbx-out", false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newOutputFixture(t)
			f.st.run.SandboxRef = c.ref
			if c.pending {
				f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now().Add(-time.Hour)}
			}

			f.srv.prepareRunOutput(t.Context(), f.run.ID, false)
			f.srv.finishRunOutput(t.Context(), f.run.ID)

			r, found := f.mem.row(f.run.ID)
			if got := found && r.CaptureGap; got != c.want {
				t.Errorf("capture gap written = %v, want %v (row %+v)", got, c.want, r)
			}
			if got := len(miscCovFinalizeData(t, f)) > 0; got != c.want {
				t.Errorf("finalize audit written = %v, want %v", got, c.want)
			}
		})
	}
}
