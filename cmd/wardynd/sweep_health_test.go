// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// healthClock is a settable clock for the fake-clock tests below.
type healthClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *healthClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *healthClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// defaultSweepFlags parses the daemon's real flags with nothing set, so the
// default intervals under test are the shipped ones.
func defaultSweepFlags(t *testing.T) *bootFlags {
	t.Helper()
	for _, k := range []string{"WARDYN_AUTOSTOP_INTERVAL", "WARDYN_APPROVAL_EXPIRY_INTERVAL", "WARDYN_RECORDING_RETENTION_DAYS"} {
		ensureUnset(t, k)
	}
	prevArgs, prevFlags := os.Args, flag.CommandLine
	t.Cleanup(func() { os.Args = prevArgs; flag.CommandLine = prevFlags })
	resetFlags(t)
	os.Args = []string{"wardynd-test"}
	return parseBootFlags()
}

// stubRunner is a non-nil runner: the registration only asks whether one exists.
type stubRunner struct{ runner.Runner }

func registeredFor(t *testing.T, in sweepInstall, clk *healthClock) *sweephealth.Tracker {
	t.Helper()
	tr := sweephealth.New(sweephealth.NewMemStore(), "r1", clk.now)
	registerSweepHealth(tr, in)
	return tr
}

func staleNames(t *testing.T, tr *sweephealth.Tracker) []string {
	t.Helper()
	st, err := tr.Status(context.Background())
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	var out []string
	for _, s := range st {
		if s.Stale {
			out = append(out, s.Name)
		}
	}
	return out
}

// runDefaultInstall ticks every registered sweep on its own schedule, minute by
// minute, for d, skipping the sweeps in stopped.
func runDefaultInstall(t *testing.T, tr *sweephealth.Tracker, clk *healthClock, d time.Duration, stopped ...string) {
	t.Helper()
	ctx := context.Background()
	sweeps := tr.Registered()
	start := clk.now()
	for clk.now().Sub(start) < d {
		clk.advance(time.Minute)
		elapsed := clk.now().Sub(start)
		for _, s := range sweeps {
			if elapsed%s.Interval != 0 || slices.Contains(stopped, s.Name) {
				continue
			}
			if err := tr.Tick(ctx, s.Name, func(context.Context) error { return nil }); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// The default install registers its sweeps at their shipped intervals, and
// leaves out the ones whose start condition does not hold: recording retention
// is off by default.
func TestSweepHealth_DefaultInstallRegistersItsSweepsAtDefaultIntervals(t *testing.T) {
	f := defaultSweepFlags(t)
	srv := api.New(api.Config{Runner: stubRunner{}, ImageBuilder: &fakeSweepImageBuilder{}})
	clk := &healthClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	tr := registeredFor(t, sweepInstall{
		runner: true, autoStop: *f.autoStopInterval, approvalExpiry: *f.approvalExpiryInterval,
		recordingSweepable: true, recordingRetentionDays: *f.recordingRetention, runOutputPersist: *f.runOutputPersist, api: srv.HealthSweeps(),
	}, clk)

	want := map[string]time.Duration{
		sweephealth.IdleReaper:       time.Minute,
		sweephealth.ApprovalExpiry:   10 * time.Minute,
		sweephealth.TerminalSandbox:  5 * time.Minute,
		sweephealth.RunSecret:        15 * time.Minute,
		sweephealth.CredentialExpiry: 24 * time.Hour,
		sweephealth.RunWatcher:       time.Minute,
		sweephealth.OrphanedBuild:    30 * time.Minute,
		sweephealth.RunOutput:        time.Minute,
	}
	got := map[string]time.Duration{}
	for _, s := range tr.Registered() {
		got[s.Name] = s.Interval
	}
	if len(got) != len(want) {
		t.Fatalf("registered %v, want %v (recording_retention is off by default and must not be registered)", got, want)
	}
	for name, iv := range want {
		if got[name] != iv {
			t.Errorf("%s registered at %s, want %s", name, got[name], iv)
		}
	}
}

// Advanced 73h with every tick succeeding, the default install reports no
// sweep_stale. With credential_expiry stopped, it reports it after 72h and
// not before, and names nothing else.
func TestSweepHealth_DefaultInstallFakeClock(t *testing.T) {
	f := defaultSweepFlags(t)
	srv := api.New(api.Config{Runner: stubRunner{}, ImageBuilder: &fakeSweepImageBuilder{}})
	in := sweepInstall{
		runner: true, autoStop: *f.autoStopInterval, approvalExpiry: *f.approvalExpiryInterval,
		recordingSweepable: true, recordingRetentionDays: *f.recordingRetention, runOutputPersist: *f.runOutputPersist, api: srv.HealthSweeps(),
	}

	t.Run("every tick succeeds", func(t *testing.T) {
		clk := &healthClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
		tr := registeredFor(t, in, clk)
		runDefaultInstall(t, tr, clk, 73*time.Hour)
		if stale := staleNames(t, tr); len(stale) != 0 {
			t.Fatalf("after 73h of clean ticks, stale = %v, want none", stale)
		}
	})

	t.Run("credential_expiry stopped", func(t *testing.T) {
		clk := &healthClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
		tr := registeredFor(t, in, clk)
		runDefaultInstall(t, tr, clk, 72*time.Hour-time.Minute, sweephealth.CredentialExpiry)
		if stale := staleNames(t, tr); len(stale) != 0 {
			t.Fatalf("one minute before 72h, stale = %v, want none", stale)
		}
		runDefaultInstall(t, tr, clk, time.Minute, sweephealth.CredentialExpiry)
		if stale := staleNames(t, tr); len(stale) != 1 || stale[0] != sweephealth.CredentialExpiry {
			t.Fatalf("at 72h with credential_expiry stopped, stale = %v, want only credential_expiry", stale)
		}
	})
}

// A default install with recording retention off and the approval sweeper
// disabled reports no sweep_stale, however long it runs: those sweeps are not
// registered, so they cannot be stale.
func TestSweepHealth_DisabledSweepsAreNeverStale(t *testing.T) {
	clk := &healthClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	tr := registeredFor(t, sweepInstall{
		runner: true, autoStop: time.Minute, approvalExpiry: 0,
		recordingSweepable: true, recordingRetentionDays: 0,
	}, clk)
	for _, s := range tr.Registered() {
		if s.Name == sweephealth.ApprovalExpiry || s.Name == sweephealth.RecordingRetention {
			t.Fatalf("%s registered although its start condition does not hold", s.Name)
		}
	}
	runDefaultInstall(t, tr, clk, 100*time.Hour)
	if stale := staleNames(t, tr); len(stale) != 0 {
		t.Fatalf("stale = %v, want none", stale)
	}
}

// A replica registers the sweeps whose condition holds on this install whether
// or not it ever holds the sweeper lock, and a retention window turns the
// recording sweep on.
func TestSweepHealth_RegistrationDoesNotDependOnTheLock(t *testing.T) {
	clk := &healthClock{t: time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)}
	tr := registeredFor(t, sweepInstall{
		runner: false, autoStop: time.Minute, approvalExpiry: time.Hour,
		recordingSweepable: true, recordingRetentionDays: 30,
	}, clk)
	got := map[string]bool{}
	for _, s := range tr.Registered() {
		got[s.Name] = true
	}
	for _, name := range []string{sweephealth.ApprovalExpiry, sweephealth.RecordingRetention, sweephealth.RunSecret, sweephealth.CredentialExpiry} {
		if !got[name] {
			t.Errorf("%s not registered", name)
		}
	}
	for _, name := range []string{sweephealth.IdleReaper, sweephealth.TerminalSandbox} {
		if got[name] {
			t.Errorf("%s registered with no runner", name)
		}
	}
}

// failingSweeps stands in for the five sweeps of this package, each with a
// dependency that fails.
type failingSweeps struct{ err error }

func (f failingSweeps) SweepRunSecrets(context.Context) (int, error)         { return 0, f.err }
func (f failingSweeps) SweepExpiredCredentials(context.Context) (int, error) { return 0, f.err }
func (f failingSweeps) Sweep(time.Duration) (int, error)                     { return 0, f.err }
func (f failingSweeps) SweepTerminalSandboxesPage(context.Context, store.Page) (int, int, error) {
	return 0, 0, f.err
}

// An erroring tick moves the attempt and never the success, for each sweep in
// this package; a clean one moves both. The sweeps run their real loops.
func TestSweepers_ErroringTickIsAnAttemptNotASuccess(t *testing.T) {
	type run func(ctx context.Context, tr *sweephealth.Tracker, f failingSweeps)
	sweeps := map[string]run{
		sweephealth.RunSecret: func(ctx context.Context, tr *sweephealth.Tracker, f failingSweeps) {
			runSecretSweeper(ctx, f, time.Millisecond, tr)
		},
		sweephealth.CredentialExpiry: func(ctx context.Context, tr *sweephealth.Tracker, f failingSweeps) {
			runCredentialSweeper(ctx, f, time.Millisecond, tr)
		},
		sweephealth.RecordingRetention: func(ctx context.Context, tr *sweephealth.Tracker, f failingSweeps) {
			runRecordingSweeper(ctx, f, nil, time.Millisecond, time.Hour, tr)
		},
		sweephealth.TerminalSandbox: func(ctx context.Context, tr *sweephealth.Tracker, f failingSweeps) {
			runTerminalSandboxSweeper(ctx, f, nil, time.Millisecond, tr)
		},
		sweephealth.ApprovalExpiry: func(ctx context.Context, tr *sweephealth.Tracker, f failingSweeps) {
			st := &sweepStore{}
			failing := listFailingApprovals{sweepStore: st, err: f.err}
			runApprovalSweeper(ctx, failing, time.Millisecond, time.Hour, nil, tr)
		},
	}
	for name, runSweep := range sweeps {
		for _, tc := range []struct {
			label string
			err   error
		}{{"erroring", errors.New("dependency down")}, {"clean", nil}} {
			t.Run(name+"/"+tc.label, func(t *testing.T) {
				ticks := sweephealth.NewMemStore()
				tr := sweephealth.New(ticks, "r1", nil)
				ctx, cancel := context.WithCancel(context.Background())
				done := make(chan struct{})
				go func() { defer close(done); runSweep(ctx, tr, failingSweeps{err: tc.err}) }()
				deadline := time.Now().Add(5 * time.Second)
				for {
					got, _ := ticks.Ticks(context.Background())
					if tk := got[name]; !tk.AttemptedAt.IsZero() && (tc.err != nil || !tk.SucceededAt.IsZero()) {
						break
					}
					if time.Now().After(deadline) {
						t.Fatal("the sweep recorded nothing")
					}
					time.Sleep(time.Millisecond)
				}
				cancel()
				<-done
				got, _ := ticks.Ticks(context.Background())
				tk := got[name]
				if tc.err != nil && !tk.SucceededAt.IsZero() {
					t.Fatalf("an erroring tick recorded a success at %s", tk.SucceededAt)
				}
				if tc.err == nil && tk.SucceededAt.IsZero() {
					t.Fatal("a clean tick recorded no success")
				}
			})
		}
	}
}

// listFailingApprovals is the approval store whose listing fails (or works).
type listFailingApprovals struct {
	*sweepStore
	err error
}

func (l listFailingApprovals) ListApprovals(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	if l.err != nil {
		return nil, l.err
	}
	return l.sweepStore.ListApprovals(ctx, state)
}
