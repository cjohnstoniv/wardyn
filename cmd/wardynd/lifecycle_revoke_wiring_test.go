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
