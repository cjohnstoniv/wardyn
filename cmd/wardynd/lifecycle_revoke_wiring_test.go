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
