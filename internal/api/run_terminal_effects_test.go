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

	_ = s.finalizeRunEffects(ctx, run.ID, "sbx", "run.complete", nil, false, false)

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
