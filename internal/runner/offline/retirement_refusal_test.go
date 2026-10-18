// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runner/runnerio"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// pendingResults runs the real Pending loop over a loopback org peer and
// returns the action results the org acknowledged, in order.
func pendingResults(t *testing.T, m *Manager, actions []runnerwire.PendingAction) []runnerwire.ActionResult {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	accepted := make(chan runnerwire.ActionResult, len(actions))
	orgConn, runConn := runnerwire.Loopback()
	org := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnEventChecked: func(event runnerwire.Event) error {
		var result runnerwire.ActionResult
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return err
		}
		accepted <- result
		return nil
	}})
	peer := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	org.Start(ctx, 0)
	peer.Start(ctx, 0)
	defer org.Close()
	defer peer.Close()
	if err := m.Pending(ctx, peer, actions); err != nil {
		t.Fatalf("Pending failed, which would keep the runner from READY: %v", err)
	}
	out := make([]runnerwire.ActionResult, 0, len(actions))
	for range actions {
		out = append(out, <-accepted)
	}
	return out
}

// otherRun is a second created run whose kill applies cleanly.
func otherRun(t *testing.T, m *Manager, id uuid.UUID) uuid.UUID {
	t.Helper()
	other := createLocal(t, m, m.state.Runs[id].Lease.TokenRenewedAt)
	if err := m.MutationFinished(t.Context(), runnerwire.MethodCreateSandbox, "", other, uuid.Nil, nil); err != nil {
		t.Fatal(err)
	}
	return other
}

func wantOutcomes(t *testing.T, got []runnerwire.ActionResult, actions []runnerwire.PendingAction, outcomes ...string) {
	t.Helper()
	for i, r := range got {
		if r.ID != actions[i].ID || r.Outcome != outcomes[i] {
			t.Fatalf("result %d = %+v, want %s for %s", i, r, outcomes[i], actions[i].ID)
		}
	}
}

// A retire_run whose receipt the runner cannot match closes as refused: the
// Pending loop goes on to the next action, and a replay repeats the receipt.
func TestPending_BadRetirementReceiptDoesNotWedgeReady(t *testing.T) {
	m, _, id, _, a := retirementFixture(t)
	other := otherRun(t, m, id)
	proof := *a.Retirement
	proof.Generation = uuid.New()
	a.Retirement = &proof
	actions := []runnerwire.PendingAction{a, pendingContainment(m, other, types.RunnerActionKill)}
	first := pendingResults(t, m, actions)
	wantOutcomes(t, first, actions, types.RunnerActionRefused, types.RunnerActionSuccess)
	if m.state.Runs[id].Retired {
		t.Fatal("a refused receipt retired the run")
	}
	if replay := pendingResults(t, m, actions); replay[0] != first[0] || replay[1] != first[1] {
		t.Fatalf("replay = %+v, want %+v", replay, first)
	}
}

// A stray action on a retired run closes as refused instead of failing every
// fresh session; actions after it still apply.
func TestPending_ActionOnRetiredRunIsRefusedNotWedged(t *testing.T) {
	m, _, id, _, a := retirementFixture(t)
	if got, err := m.apply(t.Context(), a); err != nil || got.Outcome != types.RunnerActionSuccess || !m.state.Runs[id].Retired {
		t.Fatalf("retire: %+v %v", got, err)
	}
	other := otherRun(t, m, id)
	actions := []runnerwire.PendingAction{pendingContainment(m, id, types.RunnerActionKill), pendingContainment(m, other, types.RunnerActionKill)}
	first := pendingResults(t, m, actions)
	wantOutcomes(t, first, actions, types.RunnerActionRefused, types.RunnerActionSuccess)
	if replay := pendingResults(t, m, actions); replay[0] != first[0] || replay[1] != first[1] {
		t.Fatalf("replay = %+v, want %+v", replay, first)
	}
}

// Settlement of a run's output for retirement has three answers: settled at
// its end, waiting (deferred), or never captured. A no_output receipt settles
// at 0 only for a run that recorded no capture and holds no buffer; a run
// whose captured output is gone still waits, as before.
func TestRetirement_NoOutputSettlement(t *testing.T) {
	for _, tc := range []struct {
		name     string
		noOutput bool
		end      int64
		capture  bool
		buffer   bool // the run holds an output buffer in runnerio
		want     string
	}{
		{name: "never captured", noOutput: true, want: types.RunnerActionSuccess},
		{name: "no_output but a buffer", noOutput: true, buffer: true, want: types.RunnerActionRefused},
		{name: "no_output but captured at create", noOutput: true, capture: true, want: types.RunnerActionRefused},
		{name: "no_output with an end", noOutput: true, end: 4, want: types.RunnerActionRefused},
		{name: "captured output gone", capture: true, want: types.RunnerActionDeferred},
		{name: "captured output settled", capture: true, buffer: true, end: 4, want: types.RunnerActionSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, _, id, _, a := retirementFixture(t)
			s, err := runnerio.New(runnertest.NewFake(), uuid.NewString(), t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if tc.buffer {
				s, _ = retainedOutput(t, id, 4)
			}
			m.outputSettled = s.OutputSettled
			r := m.state.Runs[id]
			r.OutputCapture = tc.capture
			m.state.Runs[id] = r
			a.Retirement.NoOutput, a.Retirement.OutputEnd = tc.noOutput, tc.end
			got, err := m.apply(t.Context(), a)
			if err != nil || got.Outcome != tc.want {
				t.Fatalf("apply = %+v %v, want %s", got, err, tc.want)
			}
			if m.state.Runs[id].Retired != (tc.want == types.RunnerActionSuccess) {
				t.Fatalf("retired = %v after %s", m.state.Runs[id].Retired, got.Outcome)
			}
		})
	}
}

// Created records whether the create handed the sandbox an output writer.
func TestCreated_RecordsOutputCapture(t *testing.T) {
	m, _, _, org := managerFixture(t)
	for _, capture := range []bool{false, true} {
		id := uuid.New()
		if err := m.Lease(t.Context(), runnerwire.Lease{RunID: id, TokenRenewedAt: org, EndsAt: org.Add(time.Hour)}); err != nil {
			t.Fatal(err)
		}
		spec := runner.SandboxSpec{RunID: id}
		if err := m.BeforeCreate(t.Context(), spec); err != nil {
			t.Fatal(err)
		}
		if capture {
			spec.ExecOutput = io.Discard
		}
		if err := m.Created(t.Context(), spec, id.String()); err != nil {
			t.Fatal(err)
		}
		if m.state.Runs[id].OutputCapture != capture {
			t.Fatalf("OutputCapture = %v, want %v", m.state.Runs[id].OutputCapture, capture)
		}
	}
}
