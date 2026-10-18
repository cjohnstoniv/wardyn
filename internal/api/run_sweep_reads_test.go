// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// narrowRunStore answers the narrow sweep reads (store.RunSweepReader) from its runs. Its ListRuns counts
// the calls, so a test sees whether a sweep still listed the whole table.
type narrowRunStore struct {
	sandboxOrphanStore
	asked [][]uuid.UUID
	err   error
}

func (s *narrowRunStore) GetRunsByID(_ context.Context, ids []uuid.UUID) ([]types.AgentRun, error) {
	s.asked = append(s.asked, ids)
	if s.err != nil {
		return nil, s.err
	}
	var out []types.AgentRun
	for _, r := range s.runs {
		if slices.Contains(ids, r.ID) {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *narrowRunStore) ListUndispatchedRuns(context.Context) ([]types.AgentRun, error) {
	return nil, nil
}
func (s *narrowRunStore) ListTerminalRunsWithSandbox(context.Context) ([]types.AgentRun, error) {
	return nil, nil
}
func (s *narrowRunStore) ListLiveRuns(context.Context) ([]types.AgentRun, error) { return nil, nil }

// The run-secret sweep reads the rows of the runs it holds or has committed, not the table: a deployment's
// run history is not in the 15-minute tick's cost.
func TestSweepRunSecrets_ReadsOnlyTheHeldRuns(t *testing.T) {
	h := newHarness(t)
	cold := agedRun(types.RunCompleted, 2*RunSecretGrace)
	live := agedRun(types.RunRunning, 2*RunSecretGrace)
	unrelated := agedRun(types.RunCompleted, 3*RunSecretGrace) // history the sweep holds nothing for
	unknown := uuid.New()

	reg := secretmask.NewRegistry()
	reg.Add(cold.ID, []byte("cold-secret-value"))
	reg.Add(live.ID, []byte("live-secret-value"))
	reg.Add(unknown, []byte("unknown-secret-value"))

	st := &narrowRunStore{sandboxOrphanStore: sandboxOrphanStore{runs: []types.AgentRun{cold, live, unrelated}}}
	cfg := baseTestConfig(h, st)
	cfg.MaskRegistry = reg
	srv := New(cfg)

	if n, err := srv.SweepRunSecrets(context.Background()); err != nil || n != 1 {
		t.Fatalf("SweepRunSecrets = %d, %v, want 1 (the cold terminal run) and no error", n, err)
	}
	if st.listCalls != 0 {
		t.Errorf("the sweep listed the whole run table %d time(s), want 0", st.listCalls)
	}
	if len(st.asked) != 1 || len(st.asked[0]) != 3 || slices.Contains(st.asked[0], unrelated.ID) {
		t.Errorf("the sweep asked for %v, want exactly the three held runs", st.asked)
	}
	for _, id := range []uuid.UUID{live.ID, unknown} {
		if len(reg.Snapshot(id)) != 1 {
			t.Errorf("run %s lost its secret: the sweep must keep what it cannot prove cold", id)
		}
	}
	if snap := reg.Snapshot(cold.ID); len(snap) != 0 {
		t.Errorf("cold run still holds %d secret(s) (%q)", len(snap), bytes.Join(snap, []byte(",")))
	}
}

// A store that answers the orphan sweep per container reaches the same verdicts as the listing did, and a
// run it cannot read is never torn down.
func TestSweepOrphanedSandboxes_ReadsEachContainersRunNotTheTable(t *testing.T) {
	h := newHarness(t)
	orphan, live, missing, unreadable := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	st := &narrowRunStore{sandboxOrphanStore: sandboxOrphanStore{runs: []types.AgentRun{
		{ID: orphan, State: types.RunFailed},
		{ID: live, State: types.RunRunning, SandboxRef: "sandbox-live", UpdatedAt: time.Now()},
	}}}
	rn := &sandboxOrphanRunner{fakeRunner: &fakeRunner{}, live: []uuid.UUID{orphan, live, missing}}
	cfg := baseTestConfig(h, st)
	cfg.Runner = rn
	srv := New(cfg)

	if err := srv.sweepOrphanedSandboxes(context.Background()); err != nil {
		t.Fatalf("sweepOrphanedSandboxes: %v", err)
	}
	if !slices.Equal(rn.tornDown, []uuid.UUID{orphan, missing}) {
		t.Errorf("torn down = %v, want the terminal ref-empty run and the row-less one", rn.tornDown)
	}
	if st.listCalls != 0 {
		t.Errorf("the sweep listed the whole run table %d time(s), want 0", st.listCalls)
	}

	st.err = errors.New("store down")
	rn.live, rn.tornDown = []uuid.UUID{unreadable}, nil
	if err := srv.sweepOrphanedSandboxes(context.Background()); err == nil {
		t.Error("an unreadable run was swept without an error")
	}
	if len(rn.tornDown) != 0 {
		t.Errorf("torn down = %v, want nothing: a run that cannot be read is not an orphan", rn.tornDown)
	}
}

// The boot reconcile's two run passes ask the store for the runs they act on, not for the table: a
// deployment with years of history pays nothing for them at boot or on the hourly tick.
func TestReconcilePasses_AskForTheirOwnRunsNotTheTable(t *testing.T) {
	h := newHarness(t)
	st := &narrowRunStore{}
	srv := New(baseTestConfig(h, st))

	if err := srv.finalizeUndispatchedRuns(context.Background()); err != nil {
		t.Fatalf("finalizeUndispatchedRuns: %v", err)
	}
	if err := srv.reconcileOrphanedSandbox(context.Background()); err != nil {
		t.Fatalf("reconcileOrphanedSandbox: %v", err)
	}
	if st.listCalls != 0 {
		t.Errorf("the passes listed the whole run table %d time(s), want 0", st.listCalls)
	}
}
