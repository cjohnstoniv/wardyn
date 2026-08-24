// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// agedRun is a run whose last state change was `age` ago — the clock
// SweepRunSecrets reads to tell a cold terminal run from a fresh one.
func agedRun(state types.RunState, age time.Duration) types.AgentRun {
	run := sweepRun(state, "")
	run.UpdatedAt = time.Now().UTC().Add(-age)
	return run
}

// TestSweepRunSecrets is the regression for W12-S1-2 / W21-S1-9: no production
// path called secretmask.Registry.Evict, so every run's plaintext credentials
// accumulated in wardynd's heap for the process lifetime. The sweep evicts a
// run that has been terminal past the grace period, and — because masking fails
// OPEN — keeps everything it cannot prove terminal-and-cold: a run that only
// just ended (late readers like the finalize audit still mask against it), a
// live run, and a held id with no row in the listing at all.
func TestSweepRunSecrets(t *testing.T) {
	h := newHarness(t)
	cold := agedRun(types.RunCompleted, 2*RunSecretGrace)
	justEnded := agedRun(types.RunFailed, time.Minute)
	live := agedRun(types.RunRunning, 2*RunSecretGrace)
	unknown := uuid.New() // held, but absent from the listing

	reg := secretmask.NewRegistry()
	secrets := map[uuid.UUID][]byte{
		cold.ID:      []byte("cold-secret-value"),
		justEnded.ID: []byte("just-ended-secret-value"),
		live.ID:      []byte("live-secret-value"),
		unknown:      []byte("unknown-secret-value"),
	}
	for id, v := range secrets {
		reg.Add(id, v)
	}

	cfg := baseTestConfig(h, &sweepStore{runs: []types.AgentRun{cold, justEnded, live}})
	cfg.MaskRegistry = reg
	srv := New(cfg)

	if n := srv.SweepRunSecrets(context.Background()); n != 1 {
		t.Fatalf("evicted = %d, want 1 (only the cold terminal run)", n)
	}
	if snap := reg.Snapshot(cold.ID); len(snap) != 0 {
		t.Errorf("cold terminal run still holds %d secret(s)", len(snap))
	}
	for _, id := range []uuid.UUID{justEnded.ID, live.ID, unknown} {
		snap := reg.Snapshot(id)
		if len(snap) != 1 || !bytes.Equal(snap[0], secrets[id]) {
			t.Errorf("run %s: snapshot = %q, want its secret kept (sweep must fail closed)", id, snap)
		}
	}

	// Idempotent: a second pass finds nothing left to evict.
	if n := srv.SweepRunSecrets(context.Background()); n != 0 {
		t.Errorf("second sweep evicted = %d, want 0", n)
	}
}

// A nil mask registry (masking not wired) makes the sweep a no-op rather than a
// panic — it is started unconditionally at boot.
func TestSweepRunSecrets_NoRegistry(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &sweepStore{runs: []types.AgentRun{agedRun(types.RunCompleted, 2*RunSecretGrace)}}))
	if n := srv.SweepRunSecrets(context.Background()); n != 0 {
		t.Fatalf("evicted = %d, want 0 with no registry", n)
	}
}
