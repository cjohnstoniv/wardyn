// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// passSweeper is a runner whose every sweep pass is pass(n, isOrphan), n counting from 1.
type passSweeper struct {
	calls int
	pass  func(n int, isOrphan func(uuid.UUID) bool) (int, error)
}

func (p *passSweeper) SweepOrphanedSandboxes(_ context.Context, _ time.Duration, isOrphan func(uuid.UUID) bool) (int, error) {
	p.calls++
	return p.pass(p.calls, isOrphan)
}

// readRunStore fails its first failReads run reads.
type readRunStore struct {
	sandboxOrphanStore
	failReads int
}

func (s *readRunStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	if s.failReads > 0 {
		s.failReads--
		return types.AgentRun{}, errors.New("pg: connection reset")
	}
	return s.sandboxOrphanStore.GetRun(ctx, id)
}

var (
	quickWaits = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	onSession  = func() bool { return true }
	// errHeld is how a runner reports a candidate it holds until its pod is proven stopped.
	errHeld = errors.New("runner: k8s: teardown orphaned run: held, NetworkPolicies kept")
)

// TestReadySweepRetriesAFailedPass (#3534): a READY pass whose sweep call fails, or whose run read
// fails, is tried again on the same session, and the retry sweeps the orphan.
func TestReadySweepRetriesAFailedPass(t *testing.T) {
	for _, tc := range []struct {
		name      string
		callErr   error
		failReads int
	}{
		{name: "sweep call fails", callErr: errors.New("runnerwire: internal")},
		{name: "run read fails", failReads: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			orphan := uuid.New()
			st := &readRunStore{sandboxOrphanStore: sandboxOrphanStore{runs: []types.AgentRun{{ID: orphan, State: types.RunFailed}}}, failReads: tc.failReads}
			srv := New(baseTestConfig(h, st))
			var torn []uuid.UUID
			sw := &passSweeper{pass: func(n int, isOrphan func(uuid.UUID) bool) (int, error) {
				if n == 1 && tc.callErr != nil {
					return 0, tc.callErr
				}
				if !isOrphan(orphan) {
					return 0, nil
				}
				torn = append(torn, orphan)
				return 1, nil
			}}
			srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, onSession, quickWaits)
			if sw.calls != 2 || len(torn) != 1 {
				t.Fatalf("calls %d torn %v: the retry did not sweep the orphan, or did not stop after", sw.calls, torn)
			}
		})
	}
}

// TestReadySweepHeldCandidateStaysHeld (#3534): a candidate the runner holds is retried on every
// wait, the runner refusing it each time; the sweep tears nothing down for it and stops at the bound.
func TestReadySweepHeldCandidateStaysHeld(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sandboxOrphanStore{}))
	run := uuid.New()
	var asked int
	sw := &passSweeper{pass: func(_ int, isOrphan func(uuid.UUID) bool) (int, error) {
		if isOrphan(run) {
			asked++
		}
		return 0, errHeld
	}}
	srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, onSession, quickWaits)
	if sw.calls != 1+len(quickWaits) || asked != sw.calls {
		t.Fatalf("calls %d asked %d, want %d passes over the held candidate", sw.calls, asked, 1+len(quickWaits))
	}
}

// TestReadySweepSweepsHeldCandidateOnceProven (#3534): the pass after the runner gets its proof of
// stop sweeps the held candidate, and the retries end there.
func TestReadySweepSweepsHeldCandidateOnceProven(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sandboxOrphanStore{}))
	run := uuid.New()
	const provenAt = 3
	var torn []uuid.UUID
	sw := &passSweeper{pass: func(n int, isOrphan func(uuid.UUID) bool) (int, error) {
		if !isOrphan(run) {
			return 0, nil
		}
		if n < provenAt {
			return 0, errHeld
		}
		torn = append(torn, run)
		return 1, nil
	}}
	srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, onSession, quickWaits)
	if sw.calls != provenAt || len(torn) != 1 {
		t.Fatalf("calls %d torn %v, want the held run swept on pass %d", sw.calls, torn, provenAt)
	}
}

// TestReadySweepRetryEndsWithTheSession (#3534): no retry runs once the session that went READY is
// replaced, or once ctx ends during a wait.
func TestReadySweepRetryEndsWithTheSession(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sandboxOrphanStore{}))
	held := func(int, func(uuid.UUID) bool) (int, error) { return 0, errHeld }

	sw := &passSweeper{pass: held}
	srv.sweepRunnerOrphansOnReady(context.Background(), uuid.New(), sw, func() bool { return false }, quickWaits)
	if sw.calls != 1 {
		t.Fatalf("session replaced: %d passes, want 1", sw.calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	sw = &passSweeper{pass: func(n int, isOrphan func(uuid.UUID) bool) (int, error) {
		cancel()
		return held(n, isOrphan)
	}}
	done := make(chan struct{})
	go func() {
		srv.sweepRunnerOrphansOnReady(ctx, uuid.New(), sw, onSession, []time.Duration{time.Hour})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("ctx ended: the sweep still waits for its retry")
	}
	if sw.calls != 1 {
		t.Fatalf("ctx ended: %d passes, want 1", sw.calls)
	}
}
