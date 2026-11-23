// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build docker

package docker

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// fastForZero makes the slow cadence apply from the first probe, so a test sees it at once.
func fastForZero(t *testing.T) {
	t.Helper()
	old := waitFastFor
	waitFastFor = 0
	t.Cleanup(func() { waitFastFor = old })
}

// TestPollExecExit_SlowsOnceTheAgentHasRunAWhile: a running exec past waitFastFor is probed at the idle cadence, not
// every pollInterval.
func TestPollExecExit_SlowsOnceTheAgentHasRunAWhile(t *testing.T) {
	fastForZero(t)
	f := newFakeDocker()
	d := newTestDriver(f)
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	if _, err := d.pollExecExit(ctx, "e", time.Second); err == nil {
		t.Fatal("a running exec reported an exit")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execInspects > 3 {
		t.Errorf("%d exec inspects in 1.5 s at a 1 s idle cadence; want at most 3", f.execInspects)
	}
}

// TestPollExecExit_FailedProbesKeepTheFastCadence: probe errors are never slowed, so the wait still reaches an exit
// that follows five of them well inside the idle cadence.
func TestPollExecExit_FailedProbesKeepTheFastCadence(t *testing.T) {
	fastForZero(t)
	f := newFakeDocker()
	f.execInspectErrs = 5
	f.execExited = true
	f.execExitCode = 7
	d := newTestDriver(f)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	code, err := d.pollExecExit(ctx, "e", time.Second)
	if err != nil || code != 7 {
		t.Fatalf("pollExecExit = %d, %v; want 7, nil within 3 s", code, err)
	}
}

// TestWaitStreamExit_KeepsTheFastCadence: a streamed exec's Wait, which a person waits on, probes at pollInterval even
// once waitFastFor has passed.
func TestWaitStreamExit_KeepsTheFastCadence(t *testing.T) {
	fastForZero(t)
	f := newFakeDocker()
	d := newTestDriver(f)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := d.waitStreamExit(ctx, "e", &atomic.Bool{}); err == nil {
		t.Fatal("a running exec reported an exit")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.execInspects < 3 {
		t.Errorf("%d exec inspects in 1 s; want at least 3", f.execInspects)
	}
}
