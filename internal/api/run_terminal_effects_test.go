// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// overrunStopRunner's StopSandbox takes a while and then fails, as a wedged daemon's does, and
// records the deadline it was given.
type overrunStopRunner struct {
	runner.Runner
	stopDeadline time.Time
}

func (r *overrunStopRunner) StopSandbox(ctx context.Context, _ string) error {
	r.stopDeadline, _ = ctx.Deadline()
	time.Sleep(20 * time.Millisecond)
	return errors.New("the daemon did not answer")
}

// deadlineAudit records the deadline each audit write was given.
type deadlineAudit struct {
	mu        sync.Mutex
	deadlines map[string]time.Time // action + "/" + outcome
}

func (a *deadlineAudit) Record(ctx context.Context, ev types.AuditEvent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	dl, _ := ctx.Deadline()
	a.deadlines[ev.Action+"/"+ev.Outcome] = dl
	return nil
}

// A write after the sandbox stop runs on a budget of its own, not the stop's: a stop that
// overruns its deadline can no longer take the teardown-error audit with it (#3482).
func TestFinalizeRunEffects_WritesAfterTheStopHaveTheirOwnBudget(t *testing.T) {
	s, _, _, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
	rn := &overrunStopRunner{Runner: sb}
	s.cfg.Runner = rn
	rec := &deadlineAudit{deadlines: map[string]time.Time{}}
	s.cfg.Audit = rec
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	_ = finalizeHeld(t, s, ctx, run.ID, "sbx", "run.complete", nil, false, false)

	rec.mu.Lock()
	audited, ok := rec.deadlines["run.complete/failure"]
	rec.mu.Unlock()
	if !ok || rn.stopDeadline.IsZero() {
		t.Fatalf("teardown-error audit recorded %v, stop deadline %v; want both", ok, rn.stopDeadline)
	}
	if !audited.After(rn.stopDeadline) {
		t.Fatalf("the teardown-error audit's deadline %v is the stop's %v, want a fresh budget after it", audited, rn.stopDeadline)
	}
}

// slowStopRunner's StopSandbox needs need: given less, it fails as a stop that overran its deadline does.
type slowStopRunner struct {
	runner.Runner
	need time.Duration
}

func (r *slowStopRunner) StopSandbox(ctx context.Context, _ string) error {
	if dl, ok := ctx.Deadline(); ok && time.Until(dl) < r.need {
		return context.DeadlineExceeded
	}
	return nil
}

// A macOS VM run's stop gets macOSVMTeardownBudget past a runner exit's 30 s, so its runner's graceful stop
// of the guest no longer overruns and audits a second run.complete as a failed teardown (#3886). A container
// run's stop keeps the caller's deadline.
func TestFinalizeRunEffects_AMacOSVMStopGetsItsOwnBudget(t *testing.T) {
	for _, tc := range []struct {
		kind        types.MachineKind
		wantFailure bool
	}{{types.MachineMacOSVM, false}, {types.MachineContainer, true}} {
		t.Run(string(tc.kind), func(t *testing.T) {
			s, cs, audit, run, sb, _ := computerFolderFixture(t, types.ComputerFolderBoth)
			cs.dispatchTestStore.mu.Lock()
			cs.run.MachineKind = tc.kind
			cs.dispatchTestStore.mu.Unlock()
			s.cfg.Runner = &slowStopRunner{Runner: sb, need: 45 * time.Second}
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()

			_ = finalizeHeld(t, s, ctx, run.ID, "sbx", "run.complete", nil, false, false)

			if got := findAudit(audit.snapshot(), run.ID, "run.complete", "failure") != nil; got != tc.wantFailure {
				t.Fatalf("a teardown-error run.complete for a %s run: %v, want %v", tc.kind, got, tc.wantFailure)
			}
		})
	}
}

// A macOS VM run's exit receipt gets the time its stop can take on top of every receipt's 30 s, so its tail
// is not cut short after the stop and run again (#3886).
func TestRunnerExitTimeout_CoversAMacOSVMStop(t *testing.T) {
	id := uuid.New()
	st := &dispatchTestStore{run: types.AgentRun{ID: id}}
	s := New(baseTestConfig(newHarness(t), st))
	if got := s.runnerExitTimeout(context.Background(), id); got != 30*time.Second {
		t.Fatalf("a container run's receipt gets %v, want 30s", got)
	}
	st.mu.Lock()
	st.run.MachineKind = types.MachineMacOSVM
	st.mu.Unlock()
	if got := s.runnerExitTimeout(context.Background(), id); got < 30*time.Second+macOSVMTeardownBudget {
		t.Fatalf("a macOS VM run's receipt gets %v, want at least 30s more than its stop's %v", got, macOSVMTeardownBudget)
	}
}
