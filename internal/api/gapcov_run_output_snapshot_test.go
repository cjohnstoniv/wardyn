// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"strings"
	"testing"
	"testing/iotest"

	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// gapCovPaneFixture is a pane fixture over the run-output store with hooks, so a
// snapshot can be driven directly against that store.
func gapCovPaneFixture(t *testing.T, pane func(runner.ExecSpec) (*runner.ExecSession, error), shape ...func(*Config)) (*paneFixture, *miscCovOutStore) {
	t.Helper()
	var cs *miscCovOutStore
	f := newPaneFixture(t, pane, append([]func(*Config){func(c *Config) {
		cs = newMiscCovOutStore(c.Store.(*memRunOutputs))
		c.Store = cs
	}}, shape...)...)
	return f, cs
}

// gapCovSnapshotFailed asserts the snapshot left no pane row and audited exactly
// one failure carrying reason (never pane content).
func gapCovSnapshotFailed(t *testing.T, f *paneFixture, reason string) {
	t.Helper()
	if r, ok := f.mem.row(f.run.ID); ok && r.Source == paneSnapshotSource {
		t.Errorf("a pane row exists: %+v", r)
	}
	evs := f.snapshotAudit()
	if len(evs) != 1 || evs[0].Outcome != "failure" || !strings.Contains(string(evs[0].Data), `"reason":"`+reason+`"`) {
		t.Fatalf("audit rows %+v, want one failure with reason %s", evs, reason)
	}
}

// A pane whose masking corpus cannot be proven whole is never executed or kept.
func TestGapCovPaneSnapshotUncoveredRunIsNotExecuted(t *testing.T) {
	f, cs := gapCovPaneFixture(t, textPane("pane text\n", 0), func(c *Config) {
		pool, _ := miscCovClosedPool(t)
		c.MaskRegistry = secretmask.NewRegistry()
		c.MaskManifests = maskmanifest.New(pool, nil, c.MaskRegistry)
	})
	f.srv.snapshotRunPane(t.Context(), cs, f.run)
	if n := len(f.pr.execs()); n != 0 {
		t.Errorf("the pane was executed %d time(s) for an uncovered run", n)
	}
	gapCovSnapshotFailed(t, f, "mask_uncovered")
}

// A stdout that fails mid-read is an exec failure and keeps nothing.
func TestGapCovPaneSnapshotStdoutReadFailureKeepsNothing(t *testing.T) {
	pane := func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{
			Stdout: iotest.ErrReader(errors.New("gapcov: stream cut")),
			Wait:   func() (int, error) { return 0, nil },
		}, nil
	}
	f, cs := gapCovPaneFixture(t, pane)
	f.srv.snapshotRunPane(t.Context(), cs, f.run)
	gapCovSnapshotFailed(t, f, "exec_failed")
}

// A session with no Wait (a driver that reports no exit) is kept once stdout ends.
func TestGapCovPaneSnapshotWithoutAWaitIsKept(t *testing.T) {
	pane := func(runner.ExecSpec) (*runner.ExecSession, error) {
		return &runner.ExecSession{Stdout: strings.NewReader("pane text\n")}, nil
	}
	f, cs := gapCovPaneFixture(t, pane)
	f.srv.snapshotRunPane(t.Context(), cs, f.run)
	r, ok := f.mem.row(f.run.ID)
	if !ok || r.Source != paneSnapshotSource || string(r.Output) != "pane text\n" {
		t.Fatalf("row = %+v (found %v), want the pane kept", r, ok)
	}
	if evs := f.snapshotAudit(); len(evs) != 1 || evs[0].Outcome != "success" {
		t.Fatalf("audit rows %+v, want one success", evs)
	}
}

// A masking corpus that cannot be proven whole while the pane is read keeps no row and is audited as
// mask_uncovered.
func TestGapCovPaneSnapshotCorpusLostWhileReadingKeepsNothing(t *testing.T) {
	reg := secretmask.NewRegistry()
	reg.SetBackend(&gapCovMaskBackend{freshErr: errors.New("gapcov: corpus unreadable")})
	f, cs := gapCovPaneFixture(t, textPane("pane text\n", 0), func(c *Config) { c.MaskRegistry = reg })
	f.srv.snapshotRunPane(t.Context(), cs, f.run)
	gapCovSnapshotFailed(t, f, "mask_uncovered")
}

// A store that refuses the row ends the snapshot with the matching reason and no row; an erased run
// also fences the run's open tail, and any other failure leaves it held.
func TestGapCovPaneSnapshotStoreRefusals(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		err          error
		tailHeld     bool
	}{
		{"erased", "erased", store.ErrRunOutputErased, false},
		{"store failure", "persist_failed", errors.New("gapcov: write refused"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, cs := gapCovPaneFixture(t, textPane("pane text\n", 0))
			f.open(t)
			cs.saveFinalFn = func(int) error { return tc.err }
			f.srv.snapshotRunPane(t.Context(), cs, f.run)
			gapCovSnapshotFailed(t, f, tc.reason)
			if held := f.srv.tailFor(f.run.ID) != nil; held != tc.tailHeld {
				t.Errorf("run's tail held = %v, want %v", held, tc.tailHeld)
			}
		})
	}
}
