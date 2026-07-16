// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killCountRunner records KillSandbox invocations so a re-kill can be observed.
type killCountRunner struct {
	*fakeRunner
	kills atomic.Int32
}

func (r *killCountRunner) KillSandbox(context.Context, string) error {
	r.kills.Add(1)
	return nil
}

// killRekillStore serves an already-KILLED run and accepts the (no-op) KILLED->
// KILLED conditional write.
type killRekillStore struct {
	store.Store
	run types.AgentRun
}

func (s *killRekillStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}
func (s *killRekillStore) UpdateRunStateIf(_ context.Context, _ uuid.UUID, from, to types.RunState) (bool, error) {
	// The re-kill CASes KILLED->KILLED; it applies (state still KILLED).
	return from == s.run.State, nil
}

// TestKillRun_RekillKilledRerunsCascade is the U040 regression: an already-KILLED
// run (its first kill's teardown may have failed and advised a retry) must be
// re-killable — the handler re-runs the idempotent KillSandbox + broker revoke
// instead of 409'ing and orphaning the sandbox/credentials forever.
func TestKillRun_RekillKilledRerunsCascade(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := &killRekillStore{run: types.AgentRun{ID: runID, State: types.RunKilled, SandboxRef: "sbx-1"}}
	rr := &killCountRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rr
	cfg.Broker = h.broker
	srv := New(cfg)

	w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")

	// Not 409 — the re-kill is allowed to proceed.
	if w.Code == http.StatusConflict {
		t.Fatalf("re-kill of a KILLED run was rejected 409; it must re-run the idempotent cascade. body=%s", w.Body.String())
	}
	if got := rr.kills.Load(); got != 1 {
		t.Errorf("KillSandbox called %d times, want 1 (the re-kill must re-run teardown)", got)
	}
	if len(h.broker.revoked) == 0 || h.broker.revoked[len(h.broker.revoked)-1] != runID {
		t.Errorf("re-kill must re-run the broker revoke cascade for %s; revoked=%v", runID, h.broker.revoked)
	}
}

// TestKillRun_NonKilledTerminalStill409 guards that the exemption is KILLED-only:
// a COMPLETED/FAILED/STOPPED/ARCHIVED run must still 409 (writing KILLED would
// corrupt the recorded outcome), and must NOT touch the runner.
func TestKillRun_NonKilledTerminalStill409(t *testing.T) {
	for _, st := range []types.RunState{types.RunCompleted, types.RunFailed, types.RunStopped, types.RunArchived} {
		h := newHarness(t)
		runID := uuid.New()
		store := &killRekillStore{run: types.AgentRun{ID: runID, State: st, SandboxRef: "sbx-1"}}
		rr := &killCountRunner{fakeRunner: &fakeRunner{}}
		cfg := baseTestConfig(h, store)
		cfg.Runner = rr
		srv := New(cfg)

		w := do(t, srv, http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
		if w.Code != http.StatusConflict {
			t.Errorf("kill of %s run: code = %d, want 409", st, w.Code)
		}
		if got := rr.kills.Load(); got != 0 {
			t.Errorf("kill of %s run touched the runner (%d KillSandbox calls); must not", st, got)
		}
	}
}
