// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// killNotStarted is a kill that asks ?not_started=true, the console's cancel of a run that has not started, sent
// to a run read at from that is at moved by the kill's claim (moved == from: no move).
func killNotStarted(t *testing.T, from, moved types.RunState, movedRef string) (*moveOnReadStore, *raceBroker, *killCountRunner, int, string) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	st := &moveOnReadStore{raceStore: raceStore{run: types.AgentRun{ID: runID}, state: from}, moved: moved, movedRef: movedRef}
	brk := &raceBroker{}
	rn := &killCountRunner{fakeRunner: &fakeRunner{}}
	cfg := baseTestConfig(h, st)
	cfg.Broker = brk
	cfg.Runner = rn
	w := do(t, New(cfg), http.MethodPost, "/api/v1/runs/"+runID.String()+"/kill?not_started=true", adminToken, "")
	return st, brk, rn, w.Code, w.Body.String()
}

// A cancel of a run that has not started ends a run that is still QUEUED, as a plain kill does.
func TestKillRun_NotStartedCancelsAQueuedRun(t *testing.T) {
	st, _, _, code, body := killNotStarted(t, types.RunQueued, types.RunQueued, "")
	if code != http.StatusAccepted {
		t.Fatalf("not_started kill of a QUEUED run = %d %s, want 202", code, body)
	}
	if got := st.State(); got != types.RunKilled {
		t.Errorf("state = %s, want KILLED", got)
	}
}

// The promotion boundary: the run is read QUEUED and promoted to PENDING before the kill's claim, so the claim
// loses. A plain kill follows the run and ends it (TestKillRun_LiveMoveUnderTheKillStillKills); a cancel of a run
// that has not started stops there with 409 run_kill_started and leaves the started run, its sandbox and its
// credentials alone. Read already started, it is refused the same way before any claim.
func TestKillRun_NotStartedRefusedOnceTheRunStarted(t *testing.T) {
	for _, tc := range []struct {
		name        string
		from, moved types.RunState
		ref         string
	}{
		{"promoted_under_the_kill", types.RunQueued, types.RunPending, ""},
		{"running_when_read", types.RunRunning, types.RunRunning, "sbx-1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, brk, rn, code, body := killNotStarted(t, tc.from, tc.moved, tc.ref)
			if code != http.StatusConflict || !strings.Contains(body, reasonRunKillStarted) {
				t.Fatalf("not_started kill, run %s -> %s = %d %s, want 409 %s", tc.from, tc.moved, code, body, reasonRunKillStarted)
			}
			if got := st.State(); got != tc.moved {
				t.Errorf("state = %s, want %s kept", got, tc.moved)
			}
			if n := brk.revocations(st.run.ID); n != 0 || rn.kills.Load() != 0 {
				t.Errorf("revocations = %d, KillSandbox calls = %d; a refused cancel must touch neither", n, rn.kills.Load())
			}
		})
	}
}

// A run already KILLED is a cancel that already happened: the flag does not refuse its re-kill.
func TestKillRun_NotStartedReKillsAKilledRun(t *testing.T) {
	if _, _, _, code, body := killNotStarted(t, types.RunKilled, types.RunKilled, ""); code != http.StatusAccepted {
		t.Fatalf("not_started kill of a KILLED run = %d %s, want 202", code, body)
	}
}
