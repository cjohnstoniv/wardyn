// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIdleReaperIsWiredToTheRevocationCascade pins the wiring
// TestPG_IdleStopRunsTheKillsRevocationCascade
// cannot see: boot hands the reaper's stopper the server's cascade.
func TestIdleReaperIsWiredToTheRevocationCascade(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "wardynd", "boot_serve.go"))
	if err != nil {
		t.Fatalf("read boot_serve.go: %v", err)
	}
	if !strings.Contains(string(b), "revoke: srv.RevokeStoppedRun") {
		t.Error("startBackgroundWorkers no longer hands lifecycleStopper the revocation cascade — " +
			"an idle-stopped run keeps its stored proxy config and resident values until a sweep")
	}
}

// TestIdleReaperIsWiredToTheEndHold pins the wiring TestPG_GracefulStopsSkipTheStopPastTheEndHold cannot see: boot
// hands the reaper's stopper the end hold's deadline check. The field is nil-safe, so without the line the idle and
// max-age stop would stop its sandbox past its end hold with no other test failing. Spacing is ignored, so gofmt's
// alignment of the literal does not matter.
func TestIdleReaperIsWiredToTheEndHold(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "wardynd", "boot_serve.go"))
	if err != nil {
		t.Fatalf("read boot_serve.go: %v", err)
	}
	if !strings.Contains(strings.Join(strings.Fields(string(b)), " "), "holdPassed: api.EndHoldPassed,") {
		t.Error("startBackgroundWorkers no longer hands lifecycleStopper api.EndHoldPassed: " +
			"an idle or max-age stop then stops its sandbox past its end hold")
	}
}

// TestIdleReaperIsWiredToTheOutageHooks pins the wiring no other test can see: boot hands the reaper the store
// health tracker, the runner sessions and the idle clock stamp. Every field is nil-safe, so without a line the
// reaper would idle-stop through a database recovery, or a run whose runner the server cannot reach, with no
// other test failing. Spacing is ignored, so gofmt's alignment of the literal does not matter.
func TestIdleReaperIsWiredToTheOutageHooks(t *testing.T) {
	read := func(name string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(repoRoot(t), "cmd", "wardynd", name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return strings.Join(strings.Fields(string(b)), " ")
	}
	boot := read("boot_serve.go")
	for _, c := range []struct{ want, consequence string }{
		{"StoreRecovering: health.Recovering,", "idle stops are no longer held while the database recovers"},
		{"StoreHealthyFor: health.HealthyFor,", "a database that goes away again inside the grace no longer restarts the idle clocks"},
		{"RunnerOnline: srv.RunnerOnline,", "a run on a runner the server cannot reach is idle-stopped"},
		{"StampIdle: lifecycleStore{pool: pool}.StampIdleClocks,", "idle clocks are never restarted after an outage"},
	} {
		if !strings.Contains(boot, c.want) {
			t.Errorf("startBackgroundWorkers no longer sets %q: %s", c.want, c.consequence)
		}
	}
	if !strings.Contains(read("main.go"), "ticks, health, gtWrite)") {
		t.Error("main.go no longer passes the store health tracker to startBackgroundWorkers: the reaper gets a nil tracker and idle stops are never held")
	}
}
