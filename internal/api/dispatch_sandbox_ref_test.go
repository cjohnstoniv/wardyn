// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refWriteStore is dispatchTestStore whose SetSandboxRef answers as scripted.
type refWriteStore struct {
	*dispatchTestStore
	errs   []error // one per call, in order; past the end the write succeeds
	lands  bool    // a write that reports an error is stored all the same
	onCall func()  // runs inside the first call
	calls  int
	ref    string
}

func (s *refWriteStore) SetSandboxRef(_ context.Context, _ uuid.UUID, ref string) error {
	s.calls++
	if s.calls == 1 && s.onCall != nil {
		s.onCall()
	}
	var err error
	if s.calls <= len(s.errs) {
		err = s.errs[s.calls-1]
	}
	if err == nil || s.lands {
		s.ref = ref
	}
	return err
}

// TestDispatch_UnrecordedSandboxRef: a sandbox whose ref the run's row does not
// hold is one no kill, sweep or other replica can reach, so dispatch never
// starts a run on it. One retry of the same write is allowed. A write that
// still fails ends the dispatch before the run reads RUNNING: the sandbox this
// dispatch still knows is stopped and the run fails from STARTING, leaving a
// kill that won in the meantime as it stands. A stop that fails is its own
// audit row, and no row says the sandbox is gone.
func TestDispatch_UnrecordedSandboxRef(t *testing.T) {
	lost := errors.New("store: set sandbox ref: connection reset")
	for _, tc := range []struct {
		name      string
		errs      []error
		lands     bool
		killed    bool
		stopErr   error
		wantState types.RunState
		wantStops int
	}{
		{name: "a retry that lands starts the run", errs: []error{lost}, wantState: types.RunRunning},
		{name: "a write that keeps failing fails the run", errs: []error{lost, lost}, wantState: types.RunFailed, wantStops: 1},
		{name: "a write that landed and still answered with an error fails the run", errs: []error{lost, lost}, lands: true,
			wantState: types.RunFailed, wantStops: 1},
		{name: "a kill that won stays KILLED", errs: []error{lost, lost}, killed: true, wantState: types.RunKilled, wantStops: 1},
		{name: "a stop that fails is recorded", errs: []error{lost, lost}, stopErr: errors.New("docker: daemon unreachable"),
			wantState: types.RunFailed, wantStops: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rn := &killRaceRunner{fakeRunner: &fakeRunner{}, stopErr: tc.stopErr}
			srv, base, audit, run := dispatchTeardownFixture(t, rn, types.RunPending)
			st := &refWriteStore{dispatchTestStore: base, errs: tc.errs, lands: tc.lands}
			if tc.killed {
				st.onCall = func() {
					base.mu.Lock()
					base.state = types.RunKilled
					base.mu.Unlock()
				}
			}
			srv.cfg.Store = st

			srv.dispatchRun(context.Background(), run, ceilingForDispatch(governanceCeiling{}, adoEntraUngraded(), bedrockCredUngraded()), dispatchParams{
				RunToken: "run-token", Image: "wardyn/claude-code:latest", Interactive: true,
				Policy: types.RunPolicySpec{MinConfinementClass: types.CC1},
			})

			if st.calls != 2 {
				t.Errorf("SetSandboxRef calls = %d, want the write and one retry", st.calls)
			}
			if got := base.State(); got != tc.wantState {
				t.Errorf("state = %s, want %s", got, tc.wantState)
			}
			if got := rn.stopCount(); got != tc.wantStops {
				t.Errorf("sandbox stops = %d, want %d", got, tc.wantStops)
			}
			started := findAudit(audit.events, run.ID, "run.interactive.start", "success") != nil
			// The stop's own failure row shares the action and outcome; the lost write is the one naming it.
			lostRow := slices.ContainsFunc(audit.events, func(ev types.AuditEvent) bool {
				return ev.Action == "run.create" && ev.Outcome == "failure" &&
					strings.Contains(string(ev.Data), `"set_sandbox_ref_error":"store: set sandbox ref: connection reset"`)
			})
			if tc.wantState == types.RunRunning {
				if !started || lostRow || st.ref == "" {
					t.Errorf("started = %t, ref = %q, failure row = %t; want the run started on its recorded ref", started, st.ref, lostRow)
				}
				return
			}
			if started {
				t.Error("the run was started on a sandbox its row does not hold")
			}
			if !lostRow {
				t.Fatalf("no run.create failure row carrying the write's error; events=%s", auditDump(audit.events, run.ID))
			}
			if revoked := len(srv.cfg.Broker.(*fakeBroker).revoked); (revoked == 1) != (tc.wantState == types.RunFailed) || revoked > 1 {
				t.Errorf("broker revocations = %d; want one exactly when this dispatch failed the run", revoked)
			}
			if ev := findTeardownError(audit.events, run.ID); (ev != nil) != (tc.stopErr != nil) {
				t.Errorf("teardown_error row = %v, want one exactly when the stop failed", ev)
			} else if ev != nil && (ev.Action != "run.create" || !strings.Contains(string(ev.Data), "daemon unreachable")) {
				t.Errorf("teardown_error row = %s %s, want run.create carrying the stop's error", ev.Action, ev.Data)
			}
		})
	}
}
