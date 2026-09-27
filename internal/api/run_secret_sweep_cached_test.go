// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSweepRunSecrets_EvictsCacheOnlyRuns: Registry.Masker caches a derived
// Masker per run id unconditionally — including for a run with no per-run
// secrets at all (a scan run, a grantless run), whose corpus is just the
// process globals. RunIDs() must include those ids, or the sweep never sees
// them and their cached entries live for the process lifetime.
func TestSweepRunSecrets_EvictsCacheOnlyRuns(t *testing.T) {
	h := newHarness(t)
	cold := agedRun(types.RunCompleted, 2*RunSecretGrace)
	live := agedRun(types.RunRunning, 2*RunSecretGrace)

	reg := secretmask.NewRegistry()
	reg.AddGlobal("", "test-credential", time.Now(), []byte("a-process-global-secret"))
	// Neither run ever registered a per-run secret. Asking for a Masker is all
	// it takes to cache one.
	_ = reg.Masker(cold.ID)
	_ = reg.Masker(live.ID)

	cfg := baseTestConfig(h, &sweepStore{runs: []types.AgentRun{cold, live}})
	cfg.MaskRegistry = reg
	srv := New(cfg)

	if n := srv.SweepRunSecrets(context.Background()); n != 1 {
		t.Fatalf("evicted = %d, want 1 (the cold terminal run's cached Masker)", n)
	}
	// Idempotent, and the live run's entry is untouched.
	if n := srv.SweepRunSecrets(context.Background()); n != 0 {
		t.Errorf("second sweep evicted = %d, want 0", n)
	}
}
