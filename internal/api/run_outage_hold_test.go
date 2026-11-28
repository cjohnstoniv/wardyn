// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestRevive_StrictSpentRefused: reviveStrictSpent refuses a run lost to an outage whose strict allowance is spent
// with 409 revive_strict_allowance_spent, answers a failed read 503 revive_claim_failed, and answers nil for every
// other run, reading nothing for one that was not lost to an outage.
func TestRevive_StrictSpentRefused(t *testing.T) {
	lostAt := time.Now().Add(-time.Hour)
	lost := func(reason types.LostReason) types.AgentRun {
		return types.AgentRun{ID: uuid.New(), State: types.RunRunning, LostAt: &lostAt, LostReason: reason}
	}
	cases := []struct {
		name       string
		run        types.AgentRun
		check      store.RecoveryCheck
		checkErr   error
		plainStore bool
		status     int // 0: nil
		reason     string
		reads      int // RunRecoveryCheck calls
	}{
		{name: "lost to an outage, allowance spent", run: lost(types.LostOutage), check: store.RecoveryCheck{StrictSpent: true},
			status: http.StatusConflict, reason: "revive_strict_allowance_spent", reads: 1},
		{name: "the read fails", run: lost(types.LostOutage), checkErr: errRuRead,
			status: http.StatusServiceUnavailable, reason: "revive_claim_failed", reads: 1},
		{name: "lost to an outage, allowance left", run: lost(types.LostOutage), reads: 1},
		{name: "not lost", run: types.AgentRun{ID: uuid.New(), State: types.RunRunning}, check: store.RecoveryCheck{StrictSpent: true}},
		{name: "lost to a reboot", run: lost(types.LostReboot), check: store.RecoveryCheck{StrictSpent: true}},
		{name: "lost when it ended", run: lost(types.LostEnded), check: store.RecoveryCheck{StrictSpent: true}},
		{name: "the store keeps no recovery facts", run: lost(types.LostOutage), plainStore: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st := newRuStore()
			st.check, st.checkErr = c.check, c.checkErr
			var srv *Server
			if c.plainStore {
				srv = New(Config{AdminToken: adminToken, Store: &ruPartialStore{}})
			} else {
				srv = New(Config{AdminToken: adminToken, Store: st})
			}
			t.Cleanup(srv.WaitBackground)

			got := srv.reviveStrictSpent(t.Context(), c.run)
			switch {
			case c.status == 0 && got != nil:
				t.Fatalf("reviveStrictSpent = %d %s %q, want nil", got.status, got.reason, got.msg)
			case c.status != 0 && got == nil:
				t.Fatalf("reviveStrictSpent = nil, want %d %s", c.status, c.reason)
			case c.status != 0 && (got.status != c.status || got.reason != c.reason):
				t.Fatalf("reviveStrictSpent = %d %s, want %d %s", got.status, got.reason, c.status, c.reason)
			}
			st.with(func(s *ruStore) {
				if len(s.checkAsked) != c.reads {
					t.Errorf("RunRecoveryCheck calls = %d, want %d", len(s.checkAsked), c.reads)
				}
				for _, recovering := range s.checkAsked {
					if recovering {
						t.Errorf("RunRecoveryCheck asked with the store tracker recovering, want false: a revive reads the run's own facts")
					}
				}
			})
		})
	}
}

// TestOutageRecoveryFor_RevokedRunnerAnswersNone: a lapsed pausing run on a runner the server would not admit now is
// not held, even though its recovery facts say it is held; the same runner claimed holds it.
func TestOutageRecoveryFor_RevokedRunnerAnswersNone(t *testing.T) {
	rid := uuid.New()
	st := newRuStore()
	st.check = store.RecoveryCheck{Held: true}
	st.runnersOn = true
	e := newRuEnv(t, st, nil, nil)
	run := types.AgentRun{ID: uuid.New(), CreatedBy: ruSub, State: types.RunRunning,
		Placement: types.PlacementLocal, RunnerID: &rid}
	if !e.srv.recoverySupported() {
		t.Fatal("the fixture server cannot recover a run token, so no run would be held")
	}

	cases := []struct {
		state types.RunnerState
		want  outageRecovery
		name  string
	}{
		{types.RunnerRevoked, outageRecoveryNone, "revoked runner"},
		{types.RunnerClaimed, outageRecoveryHold, "claimed runner (control)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			st.with(func(s *ruStore) {
				s.runner = types.Runner{ID: rid, State: c.state, OwnerKind: types.RunnerOwnerPerson, Owner: ruSub}
				s.calls = nil
			})
			if got := e.srv.outageRecoveryFor(t.Context(), run, lapseVerdict{}, false); got != c.want {
				t.Errorf("outageRecoveryFor = %d, want %d", got, c.want)
			}
			if !st.called("GetRunner") {
				t.Errorf("the runner was not read: the answer did not come from the runner's state")
			}
		})
	}
}
