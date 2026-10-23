// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// moveOnReadStore is the raceStore with a move landed right after the kill's read: the kill reads the run at
// its first state, and by its CAS the run is at moved, as when a dispatch or a completion lands in that gap.
type moveOnReadStore struct {
	raceStore
	moved    types.RunState
	movedRef string
	once     sync.Once
}

func (s *moveOnReadStore) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	r, err := s.raceStore.GetRun(ctx, id)
	s.once.Do(func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.state, s.run.SandboxRef = s.moved, s.movedRef
	})
	return r, err
}

func killUnderMove(t *testing.T, from, moved types.RunState, movedRef string) (*moveOnReadStore, *raceBroker, *killCountRunner, int, string) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	st := &moveOnReadStore{raceStore: raceStore{run: types.AgentRun{ID: runID}, state: from}, moved: moved, movedRef: movedRef}
	brk := &raceBroker{}
	rn := &killCountRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Broker = brk
	cfg.Runner = rn
	w := do(t, New(cfg), http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill", adminToken, "")
	return st, brk, rn, w.Code, w.Body.String()
}

// #3542: a kill sent while the run moves from one live state to the next is a kill of a live run, so it
// kills it (202), from the state the run now holds: a run that came up under the kill has its sandbox torn
// down too.
func TestKillRun_LiveMoveUnderTheKillStillKills(t *testing.T) {
	for _, tc := range []struct {
		from, moved types.RunState
		ref         string
		kills       int32
	}{
		{types.RunQueued, types.RunPending, "", 0},
		{types.RunPending, types.RunStarting, "", 0},
		{types.RunStarting, types.RunRunning, "sbx-1", 1},
	} {
		t.Run(string(tc.from)+"_to_"+string(tc.moved), func(t *testing.T) {
			st, brk, rn, code, body := killUnderMove(t, tc.from, tc.moved, tc.ref)
			if code != http.StatusAccepted {
				t.Fatalf("kill while the run moved %s -> %s = %d %s, want 202", tc.from, tc.moved, code, body)
			}
			if got := st.State(); got != types.RunKilled {
				t.Errorf("state = %s, want KILLED", got)
			}
			if n := brk.revocations(st.run.ID); n != 1 {
				t.Errorf("revocations = %d, want 1", n)
			}
			if n := rn.kills.Load(); n != tc.kills {
				t.Errorf("KillSandbox calls = %d, want %d for the sandbox the claimed run holds (%q)", n, tc.kills, tc.ref)
			}
		})
	}
}

// A run that ended under the kill keeps the outcome it recorded, and the kill answers what it answers
// today: 409 run_kill_state_changed, nothing revoked, nothing torn down. A run another kill took is the same.
func TestKillRun_EndUnderTheKillKeepsItsOutcome(t *testing.T) {
	for _, moved := range []types.RunState{types.RunCompleted, types.RunFailed, types.RunKilled} {
		t.Run(string(moved), func(t *testing.T) {
			st, brk, rn, code, body := killUnderMove(t, types.RunRunning, moved, "sbx-1")
			if code != http.StatusConflict || !strings.Contains(body, reasonRunKillStateChanged) {
				t.Fatalf("kill while the run moved RUNNING -> %s = %d %s, want 409 %s", moved, code, body, reasonRunKillStateChanged)
			}
			if got := st.State(); got != moved {
				t.Errorf("state = %s, want %s kept", got, moved)
			}
			if n := brk.revocations(st.run.ID); n != 0 || rn.kills.Load() != 0 {
				t.Errorf("revocations = %d, KillSandbox calls = %d; a kill that lost to an end must touch neither", n, rn.kills.Load())
			}
		})
	}
}
