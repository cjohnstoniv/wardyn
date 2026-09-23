// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSweepRunSecrets_EvictsCacheOnlyRuns pins B11b-F8. Registry.Masker caches
// a derived Masker per run id UNCONDITIONALLY — including for a run with no
// per-run secrets at all (a scan run, a grantless run), whose corpus is just
// the process globals. RunIDs() iterated the per-run map only, so the sweep
// never saw those ids and their cached entries lived for the process lifetime:
// the W12-S1-2 leak, reintroduced one field over.
func TestSweepRunSecrets_EvictsCacheOnlyRuns(t *testing.T) {
	h := newHarness(t)
	cold := agedRun(types.RunCompleted, 2*RunSecretGrace)
	live := agedRun(types.RunRunning, 2*RunSecretGrace)

	reg := secretmask.NewRegistry()
	reg.AddGlobal("", "test-credential", []byte("a-process-global-secret"))
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
