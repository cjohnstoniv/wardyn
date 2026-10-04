// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// gapCovOutStore is the run-output store with the two reads that decide whether a
// capture is owed made to fail on demand.
type gapCovOutStore struct {
	*miscCovOutStore
	getOutErr error
	insertErr error
}

func (s *gapCovOutStore) GetRunOutput(ctx context.Context, id uuid.UUID) (store.RunOutput, bool, error) {
	if s.getOutErr != nil {
		return store.RunOutput{}, false, s.getOutErr
	}
	return s.miscCovOutStore.GetRunOutput(ctx, id)
}

func (s *gapCovOutStore) InsertPendingRunOutput(ctx context.Context, id uuid.UUID) error {
	if s.insertErr != nil {
		return s.insertErr
	}
	return s.miscCovOutStore.InsertPendingRunOutput(ctx, id)
}

// gapCovRecoveryFixture is recoveryFixture over the failing store, with a closed
// Postgres behind the masking manifests when uncovered is set (so no run is
// covered).
func gapCovRecoveryFixture(t *testing.T, rr *recoveringRunner, uncovered bool) (*outputFixture, *gapCovOutStore) {
	t.Helper()
	var gs *gapCovOutStore
	f := newOutputFixture(t, func(c *Config) {
		gs = &gapCovOutStore{miscCovOutStore: newMiscCovOutStore(c.Store.(*memRunOutputs))}
		c.Store, c.Runner = gs, rr
		if uncovered {
			pool, _ := miscCovClosedPool(t)
			c.MaskRegistry = secretmask.NewRegistry()
			c.MaskManifests = maskmanifest.New(pool, nil, c.MaskRegistry)
		}
	})
	f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", ClaimedAt: time.Now().Add(-time.Hour)}
	return f, gs
}

// A run whose masking corpus cannot be proven whole is not read back from the
// substrate: its row is a capture gap with the mask_uncovered reason.
func TestGapCovRecoverRunOutputUncoveredRunIsAGapAndNotRead(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "must never be stored\n"
	f, gs := gapCovRecoveryFixture(t, rr, true)

	f.srv.recoverRunOutput(t.Context(), gs, f.run, "no_tail")

	if r, ok := f.mem.row(f.run.ID); !ok || !r.CaptureGap || len(r.Output) != 0 {
		t.Fatalf("row = %+v (found %v), want an empty capture gap", r, ok)
	}
	if got := f.gapReason(t); got != "mask_uncovered" {
		t.Errorf("gap reason = %q, want mask_uncovered", got)
	}
	if rr.recoverCount() != 0 || f.srv.tailFor(f.run.ID) != nil {
		t.Errorf("%d substrate read(s), tail open %v; want neither for an uncovered run", rr.recoverCount(), f.srv.tailFor(f.run.ID) != nil)
	}
}

// A run that keeps no output tail (a harness-login run) turns the recovery into a
// mask_uncovered gap instead of reading the substrate.
func TestGapCovRecoverRunOutputWithNoTailToOpenIsAGap(t *testing.T) {
	rr := newRecoveringRunner()
	f, gs := gapCovRecoveryFixture(t, rr, false)
	run := f.run
	run.Task = harnessLoginTask

	f.srv.recoverRunOutput(t.Context(), gs, run, "no_tail")

	if r, ok := f.mem.row(f.run.ID); !ok || !r.CaptureGap {
		t.Fatalf("row = %+v (found %v), want a capture gap", r, ok)
	}
	if got := f.gapReason(t); got != "mask_uncovered" {
		t.Errorf("gap reason = %q, want mask_uncovered", got)
	}
	if rr.recoverCount() != 0 {
		t.Errorf("%d substrate read(s), want none", rr.recoverCount())
	}
}

// An output row that cannot be read leaves the capture alone: no recovery, no gap
// row, nothing audited.
func TestGapCovRecoverRunOutputUnreadableRowChangesNothing(t *testing.T) {
	rr := newRecoveringRunner()
	f, gs := gapCovRecoveryFixture(t, rr, false)
	gs.getOutErr = errors.New("gapcov: output row unreadable")
	logs := miscCovCaptureLogs(t)

	f.srv.recoverRunOutput(t.Context(), gs, f.run, "no_tail")

	if _, ok := logs.find("could not read a run's output row before recovering it"); !ok {
		t.Error("the unreadable row was not logged")
	}
	if r, _ := f.mem.row(f.run.ID); r.CaptureGap || r.CapturedAt != nil {
		t.Errorf("row = %+v, want it left pending", r)
	}
	if rr.recoverCount() != 0 || gs.gapCalls != 0 || len(miscCovFinalizeData(t, f)) != 0 {
		t.Errorf("%d read(s), %d gap write(s), %d audit row(s); want none", rr.recoverCount(), gs.gapCalls, len(miscCovFinalizeData(t, f)))
	}
}

// A live adoption starts nothing, and reads nothing from the substrate, when the
// run cannot be covered, is not recordable, is interactive, has its capture
// committed already, or cannot be read.
func TestGapCovResumeRunOutputStartsNothingForARunItCannotCapture(t *testing.T) {
	committed := time.Now()
	cases := []struct {
		name      string
		uncovered bool
		shape     func(f *outputFixture, gs *gapCovOutStore)
	}{
		{"run unreadable", false, func(_ *outputFixture, gs *gapCovOutStore) { gs.getRunErr = errors.New("gapcov: run unreadable") }},
		{"interactive run", false, func(f *outputFixture, _ *gapCovOutStore) { f.st.run.Interactive = true }},
		{"harness login run", false, func(f *outputFixture, _ *gapCovOutStore) { f.st.run.Task = harnessLoginTask }},
		{"capture already committed", false, func(f *outputFixture, _ *gapCovOutStore) {
			f.mem.rows[f.run.ID] = store.RunOutput{RunID: f.run.ID, Source: "stdout", CapturedAt: &committed}
		}},
		{"output row unreadable", false, func(_ *outputFixture, gs *gapCovOutStore) { gs.getOutErr = errors.New("gapcov: row unreadable") }},
		{"masking corpus not proven", true, func(*outputFixture, *gapCovOutStore) {}},
		{"the output tombstone appears at open", false, func(_ *outputFixture, gs *gapCovOutStore) { gs.insertErr = store.ErrRunOutputErased }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rr := newRecoveringRunner()
			f, gs := gapCovRecoveryFixture(t, rr, c.uncovered)
			c.shape(f, gs)

			f.srv.resumeRunOutput(t.Context(), f.run.ID, "sbx-out")
			f.srv.WaitBackground()

			if f.srv.tailFor(f.run.ID) != nil {
				t.Error("a tail was opened")
			}
			if rr.recoverCount() != 0 {
				t.Errorf("the substrate was read %d time(s), want none", rr.recoverCount())
			}
		})
	}
}

// A substrate that cannot give the output back is not adopted: no tail, no read.
func TestGapCovResumeRunOutputNeedsASubstrateThatCanRecover(t *testing.T) {
	f := newOutputFixture(t) // its runner keeps no output log to read back
	f.srv.resumeRunOutput(t.Context(), f.run.ID, "sbx-out")
	if f.srv.tailFor(f.run.ID) != nil || f.rn.readCount() != 0 {
		t.Fatalf("tail open %v, %d substrate read(s); want neither", f.srv.tailFor(f.run.ID) != nil, f.rn.readCount())
	}
}

// The control for the refusals above: the same fixture, with nothing wrong,
// adopts the run and reads its output back once.
func TestGapCovResumeRunOutputAdoptsACoverableRun(t *testing.T) {
	rr := newRecoveringRunner()
	rr.log = "adopted output\n"
	f, _ := gapCovRecoveryFixture(t, rr, false)

	f.srv.resumeRunOutput(t.Context(), f.run.ID, "sbx-out")
	f.srv.WaitBackground()

	if rr.recoverCount() != 1 || f.srv.tailFor(f.run.ID) == nil {
		t.Fatalf("%d substrate read(s), tail open %v; want one read into an open tail", rr.recoverCount(), f.srv.tailFor(f.run.ID) != nil)
	}
	if v, kept := f.srv.readExecOutput(f.run.ID, 1<<20, false); !kept || string(v.out) != rr.log {
		t.Fatalf("tail holds %q (kept %v), want %q", v.out, kept, rr.log)
	}
}
