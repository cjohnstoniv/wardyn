// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerspool"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// Created(A) publishes the live-activity epoch into every run's spool, including a run that is still being
// created and whose spool holds only its lock. That must not make the new run's spool unopenable.
func TestCreatedDoesNotWedgeACreatingRunsSpool(t *testing.T) {
	sub := &localSub{status: runner.Status{State: types.RunRunning}}
	f := connectWireManager(t, t.TempDir(), sub, uuid.UUID.String, false)
	m := f.manager
	ctx := context.Background()

	b := uuid.New()
	if err := m.Lease(ctx, runnerwire.Lease{RunID: b, TokenRenewedAt: f.orgTime, EndsAt: f.orgTime.Add(3 * time.Hour)}); err != nil {
		t.Fatal(err)
	}
	if err := m.BeforeCreate(ctx, runner.SandboxSpec{RunID: b}); err != nil {
		t.Fatal(err)
	}
	// B's Open is partway: its directory and lock exist, its manifest does not.
	dir := m.activityDir(b)
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lock"), nil, 0600); err != nil {
		t.Fatal(err)
	}

	createLocal(t, m, f.orgTime) // Created(A)

	if _, err := os.Stat(filepath.Join(dir, "live-epoch.json")); err != nil {
		t.Fatalf("the epoch was not published into the creating run's spool: %v", err)
	}
	s, err := runnerspool.Open(dir, b)
	if err != nil {
		t.Fatalf("a creating run's spool is wedged by another run's Created: %v", err)
	}
	s.Close()
}
