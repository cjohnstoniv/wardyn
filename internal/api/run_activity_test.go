// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/lifecycle"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// fleetStore is pauseStore over many runs: the candidate list is the runs it
// holds, and TouchRun is the PG guard (a terminal run is refused, nothing moves).
type fleetStore struct {
	*pauseStore
	fmu     sync.Mutex
	runs    []types.AgentRun
	touched map[uuid.UUID]int
}

func (s *fleetStore) ListPauseCandidates(context.Context) ([]store.PauseCandidate, time.Time, error) {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	out := make([]store.PauseCandidate, 0, len(s.runs))
	for _, r := range s.runs {
		out = append(out, store.PauseCandidate{Run: r})
	}
	return out, time.Time{}, nil
}

func (s *fleetStore) TouchRun(_ context.Context, id uuid.UUID) error {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	for i, r := range s.runs {
		if r.ID != id {
			continue
		}
		if r.State != types.RunRunning {
			return store.ErrNotFound
		}
		s.runs[i].UpdatedAt = time.Now().UTC()
		s.touched[id]++
		return nil
	}
	return store.ErrNotFound
}

func (s *fleetStore) updatedAt(id uuid.UUID) time.Time {
	s.fmu.Lock()
	defer s.fmu.Unlock()
	for _, r := range s.runs {
		if r.ID == id {
			return r.UpdatedAt
		}
	}
	return time.Time{}
}

// newFleet is n RUNNING runs, each past half its auto-stop (so each is a CPU
// candidate), served by rn through a sweep over a store that lists them all.
func newFleet(t *testing.T, n int, rn *pauseRunner) (*Server, *fleetStore) {
	t.Helper()
	h := newHarness(t)
	fs := &fleetStore{
		pauseStore: &pauseStore{dispatchTestStore: &dispatchTestStore{state: types.RunRunning}},
		touched:    map[uuid.UUID]int{},
	}
	for i := range n {
		run := newFinalizeRun()
		run.SandboxRef = fmt.Sprintf("ref-%03d", i)
		run.AutoStopAfterSec = 600
		run.UpdatedAt = time.Now().UTC().Add(-9 * time.Minute)
		fs.runs = append(fs.runs, run)
	}
	cfg := baseTestConfig(h, fs)
	cfg.Runner = rn
	return New(cfg), fs
}

func newSampler(batch bool, cpu *float64) *pauseRunner {
	return &pauseRunner{
		fakeRunner: &fakeRunner{}, freeze: map[types.ConfinementClass]bool{types.CC1: true},
		batch: batch, cpu: cpu,
	}
}

// TestRunActivity_OneSubstrateReadPerTick: a tick over 50 runs makes at most one
// metrics call on a batching substrate and at most idleSamplesPerTick reads on
// one that charges per run, and a busy reading bumps the run's idle clock.
func TestRunActivity_OneSubstrateReadPerTick(t *testing.T) {
	for _, tc := range []struct {
		name      string
		batch     bool
		wantCalls int
	}{
		{"batching substrate", true, 1},
		{"per-run substrate", false, idleSamplesPerTick},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn := newSampler(tc.batch, pct(40))
			srv, fs := newFleet(t, 50, rn)
			if err := srv.sweepRunPauses(context.Background()); err != nil {
				t.Fatalf("sweepRunPauses: %v", err)
			}
			calls, refs := rn.samples()
			if calls != tc.wantCalls {
				t.Errorf("sampler calls = %d, want %d", calls, tc.wantCalls)
			}
			wantRefs := 50
			if !tc.batch {
				wantRefs = idleSamplesPerTick
			}
			if refs != wantRefs || len(fs.touched) != wantRefs {
				t.Errorf("refs read = %d, runs touched = %d, want %d of each", refs, len(fs.touched), wantRefs)
			}
		})
	}
}

// TestRunActivity_RoundRobinReadsEveryRun: the cursor moves on each tick, so a
// per-run substrate reads the whole fleet in turn and never starves the tail.
func TestRunActivity_RoundRobinReadsEveryRun(t *testing.T) {
	rn := newSampler(false, pct(40))
	srv, fs := newFleet(t, 10, rn)
	// Candidates stay candidates: put the clock back after every touch.
	for tick := 0; tick < 3; tick++ {
		if err := srv.sweepRunPauses(context.Background()); err != nil {
			t.Fatalf("sweepRunPauses: %v", err)
		}
		fs.fmu.Lock()
		for i := range fs.runs {
			fs.runs[i].UpdatedAt = time.Now().UTC().Add(-9 * time.Minute)
		}
		fs.fmu.Unlock()
	}
	if len(fs.touched) != 10 {
		t.Fatalf("runs read over 3 ticks = %d of 10: %v", len(fs.touched), fs.touched)
	}
	for id, n := range fs.touched {
		if n != 1 && n != 2 {
			t.Errorf("run %s read %d times, want 1 or 2 (fair rotation)", id, n)
		}
	}
}

// TestRunActivity_BusyRunIsNeitherPausedNorStopped: one reading decides both. A
// run that is a pause candidate AND an auto-stop candidate is read once; busy,
// it is not paused and the reaper, which reads updated_at, does not stop it;
// quiet, the control, it is paused and the reaper stops it. CPU-only work moves
// updated_at and not active_at.
func TestRunActivity_BusyRunIsNeitherPausedNorStopped(t *testing.T) {
	for _, tc := range []struct {
		name         string
		cpu          float64
		paused, stop bool
	}{
		{"busy", 40, false, false},
		{"quiet", 2, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newPauseFixture(t, time.Hour)
			f.st.run.RunLimits.PauseIdleAfterSec = 60
			f.st.run.AutoStopAfterSec = 600
			f.st.run.UpdatedAt = time.Now().UTC().Add(-time.Hour)
			rn := newSampler(false, pct(tc.cpu))
			f.srv.cfg.Runner = rn
			fs := &fleetStore{
				pauseStore: f.st, touched: map[uuid.UUID]int{},
				runs: []types.AgentRun{f.st.run},
			}
			f.srv.cfg.Store = fs
			activeBefore := *f.st.run.ActiveAt

			f.sweep(t)

			if calls, _ := rn.samples(); calls != 1 {
				t.Errorf("sampler calls = %d, want one reading for both decisions", calls)
			}
			if pausedAt, _ := f.st.paused(); (pausedAt != nil) != tc.paused {
				t.Errorf("paused = %v, want %v", pausedAt != nil, tc.paused)
			}
			if got := f.st.run.ActiveAt; !got.Equal(activeBefore) {
				t.Errorf("active_at moved to %v by a CPU reading; the presence clock is not this signal's", got)
			}

			stopper := &recordingStopper{}
			reaper := lifecycle.New(fleetLifecycle{fs}, stopper, discardRecorder{}, lifecycle.Config{})
			reaper.Tick(context.Background())
			if stopped := len(stopper.ids) == 1; stopped != tc.stop {
				t.Errorf("reaper stopped the run = %v, want %v (updated_at age %v)",
					stopped, tc.stop, time.Since(fs.updatedAt(f.run.ID)).Round(time.Minute))
			}
		})
	}
}

// fleetLifecycle adapts fleetStore to lifecycle.Store the way the PG adapter
// does: RUNNING runs with their auto-stop and updated_at.
type fleetLifecycle struct{ *fleetStore }

func (l fleetLifecycle) ListRunningWithPolicy(context.Context) ([]lifecycle.RunSummary, time.Time, error) {
	l.fmu.Lock()
	defer l.fmu.Unlock()
	var out []lifecycle.RunSummary
	for _, r := range l.runs {
		if r.State == types.RunRunning {
			out = append(out, lifecycle.RunSummary{ID: r.ID, UpdatedAt: r.UpdatedAt, PolicyAutoStopAfterSec: r.AutoStopAfterSec})
		}
	}
	return out, time.Time{}, nil
}

type recordingStopper struct{ ids []uuid.UUID }

func (s *recordingStopper) StopRun(_ context.Context, id uuid.UUID, _ time.Time) (lifecycle.StopOutcome, error) {
	s.ids = append(s.ids, id)
	return lifecycle.StopOutcome{Applied: true}, nil
}

type discardRecorder struct{}

func (discardRecorder) Record(context.Context, types.AuditEvent) error { return nil }

// TestRunActivity_ABusyReadingOnATerminalRunChangesNothing: a run that ended
// after the sweep listed it is refused by TouchRun's own guard, so a busy
// reading does not move its updated_at (the clock the killed-run tail-upload
// grace is measured from); and a run already terminal is never listed.
func TestRunActivity_ABusyReadingOnATerminalRunChangesNothing(t *testing.T) {
	rn := newSampler(true, pct(90))
	srv, fs := newFleet(t, 2, rn)
	fs.runs[0].State = types.RunKilled
	fs.runs[1].State = types.RunStopped
	stale := fs.runs[0].UpdatedAt
	if err := srv.sweepRunPauses(context.Background()); err != nil {
		t.Fatalf("sweepRunPauses: %v", err)
	}
	if len(fs.touched) != 0 || !fs.updatedAt(fs.runs[0].ID).Equal(stale) {
		t.Errorf("touched %v, updated_at %v -> %v; a terminal run must not be given a longer life",
			fs.touched, stale, fs.updatedAt(fs.runs[0].ID))
	}
}

// TestRunActivity_NoReadingIsNotAnything: with no reading (the metrics API
// down, an unreadable run) nothing is touched, so idleness behaves as it did
// before the signal: the reaper stops the run.
func TestRunActivity_NoReadingIsNotAnything(t *testing.T) {
	for name, rn := range map[string]*pauseRunner{
		"unavailable": {fakeRunner: &fakeRunner{}, batch: true, cpuErr: runner.ErrActivityUnavailable},
		"failed read": {fakeRunner: &fakeRunner{}, batch: true, cpuErr: errors.New("boom")},
		"unreadable":  {fakeRunner: &fakeRunner{}, batch: true},
	} {
		t.Run(name, func(t *testing.T) {
			srv, fs := newFleet(t, 3, rn)
			for i := range fs.runs {
				fs.runs[i].UpdatedAt = time.Now().UTC().Add(-20 * time.Minute) // past its auto-stop
			}
			if err := srv.sweepRunPauses(context.Background()); err != nil {
				t.Fatalf("sweepRunPauses: %v", err)
			}
			if len(fs.touched) != 0 {
				t.Errorf("touched %v on a missing reading", fs.touched)
			}
			stopper := &recordingStopper{}
			lifecycle.New(fleetLifecycle{fs}, stopper, discardRecorder{}, lifecycle.Config{}).Tick(context.Background())
			if len(stopper.ids) != 3 {
				t.Errorf("reaper stopped %d of 3 idle runs, want all 3 as before the signal", len(stopper.ids))
			}
		})
	}
}

// TestRunActivity_RunNotNearItsStopIsNotRead: a young run costs no reading.
func TestRunActivity_RunNotNearItsStopIsNotRead(t *testing.T) {
	rn := newSampler(true, pct(90))
	srv, fs := newFleet(t, 2, rn)
	fs.runs[0].UpdatedAt = time.Now().UTC().Add(-time.Minute)
	fs.runs[1].AutoStopAfterSec = 0
	if err := srv.sweepRunPauses(context.Background()); err != nil {
		t.Fatalf("sweepRunPauses: %v", err)
	}
	if calls, _ := rn.samples(); calls != 0 {
		t.Errorf("sampler calls = %d, want 0 for a young run and one with no auto-stop", calls)
	}
}

// TestIdleCPUSignalRow: the row is absent unless auto-stop is in use, ok while
// the substrate reports CPU, and the warn with its fix when it cannot.
func TestIdleCPUSignalRow(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name     string
		rn       *pauseRunner
		autoStop int
		want     string // "" = no row
	}{
		{"auto-stop off", newSampler(true, nil), 0, ""},
		{"signal on", newSampler(true, nil), 3600, "ok"},
		{"signal off", &pauseRunner{fakeRunner: &fakeRunner{}, batch: true, cpuErr: runner.ErrActivityUnavailable}, 3600, "warn"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := newFleet(t, 0, tc.rn)
			srv.cfg.DefaultPolicy.AutoStopAfterSec = tc.autoStop
			row, ok := srv.idleCPUSignalRow(ctx)
			if (tc.want != "") != ok || row.Status != tc.want {
				t.Fatalf("row = %+v (shown %v), want status %q", row, ok, tc.want)
			}
		})
	}
	t.Run("a run with auto-stop shows it though the default has none", func(t *testing.T) {
		srv, _ := newFleet(t, 1, newSampler(true, pct(0)))
		if _, ok := srv.idleCPUSignalRow(ctx); ok {
			t.Fatal("row shown before any run asked for auto-stop")
		}
		if err := srv.sweepRunPauses(ctx); err != nil {
			t.Fatal(err)
		}
		if _, ok := srv.idleCPUSignalRow(ctx); !ok {
			t.Error("row not shown after a run with auto-stop was seen")
		}
	})
	t.Run("a runner with no sampler has no row", func(t *testing.T) {
		srv, _ := newFleet(t, 0, newSampler(true, nil))
		srv.cfg.Runner = &fakeRunner{}
		srv.cfg.DefaultPolicy.AutoStopAfterSec = 3600
		if _, ok := srv.idleCPUSignalRow(ctx); ok {
			t.Error("row shown for a runner that cannot report CPU")
		}
	})
}

// TestIdleCPUSignalCheck pins M10's strings byte for byte
// (SETUP_CHECK.IDLE_CPU_SIGNAL).
func TestIdleCPUSignalCheck(t *testing.T) {
	ok := idleCPUSignalCheck(false)
	if ok.ID != "idle_cpu_signal" || ok.Label != "Idle detection" || ok.Status != "ok" || ok.Fix != "" ||
		ok.Detail != "Idle auto-stop counts CPU work inside a sandbox, so a run that is busy but quiet isn't stopped." {
		t.Errorf("ok row = %+v", ok)
	}
	off := idleCPUSignalCheck(true)
	if off.ID != "idle_cpu_signal" || off.Label != "Idle detection" || off.Status != "warn" ||
		off.Detail != "Wardyn can't read this cluster's metrics API, so idle auto-stop sees only attaches and network traffic. A run busy inside its sandbox with neither can be stopped as idle." ||
		off.Fix != "Install metrics-server. If it is installed, check that the runner Role allows `list` on `pods` in `metrics.k8s.io` (the chart adds it)." {
		t.Errorf("off row = %+v", off)
	}
}

func (s *recordingStopper) StopRunMaxAge(_ context.Context, id uuid.UUID, _ time.Time) (lifecycle.StopOutcome, error) {
	s.ids = append(s.ids, id)
	return lifecycle.StopOutcome{Applied: true}, nil
}
