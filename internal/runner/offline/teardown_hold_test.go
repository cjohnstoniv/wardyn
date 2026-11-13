// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// A queued kill the substrate could not see finish (a macOS VM helper that outlived its kill) is refused
// with a receipt and held, not failed: a failed Pending ends every fresh session before READY, which kept
// the runner from its other runs. The kill queued behind it applies, the session reaches READY, and each
// later session's Pending kills the held run again until the kill finishes, which drops the hold.
func TestPendingKillNotKnownToFinishIsHeldAndTheRunnerReachesReady(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	held, other := createLocal(t, m, org), createLocal(t, m, org)
	m.Offline(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	peer, receipts := receiptPeer(ctx, t)

	sub.killErr = &runner.TeardownUnconfirmedError{Err: errors.New("docker: the macOS VM helper is still running")}
	kill := pendingContainment(m, held, types.RunnerActionKill)
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{kill}); err != nil {
		t.Fatalf("a kill not known to finish failed the session before READY: %v", err)
	}
	if got := <-receipts; got.ID != kill.ID || got.Outcome != types.RunnerActionRefused || got.Reason == "" {
		t.Fatalf("receipt %+v, want a refused receipt for %s", got, kill.ID)
	}
	if h := m.state.Runs[held].TeardownUnconfirmed; h == nil || h.Kind != types.RunnerActionKill {
		t.Fatalf("hold = %+v, want the kill held", h)
	}
	if err := m.Pending(ctx, peer, nil); err != nil {
		t.Fatalf("a session with the helper still running failed before READY: %v", err)
	}
	if h := m.state.Runs[held].TeardownUnconfirmed; h == nil || sub.kills != 2 {
		t.Fatalf("with the helper still running: hold %+v, %d kills; want the hold kept and 2", h, sub.kills)
	}
	sub.killErr = nil
	next := pendingContainment(m, other, types.RunnerActionKill)
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{next}); err != nil {
		t.Fatal(err)
	}
	if got := <-receipts; got.ID != next.ID || got.Outcome == types.RunnerActionRefused {
		t.Fatalf("receipt %+v, want the other run's kill applied", got)
	}
	if err := m.BeforeReady(ctx, peer, false); err != nil {
		t.Fatalf("session did not reach READY: %v", err)
	}
	if h := m.state.Runs[held].TeardownUnconfirmed; h != nil || sub.kills != 4 {
		t.Fatalf("after the helper exited: hold %+v, %d kills; want none and 4 (the held run three times, the other once)", h, sub.kills)
	}
}

// heldEndSub keeps an ended sandbox and counts its ends apart from its kills.
type heldEndSub struct {
	*localSub
	ends   int
	endErr error
}

func (s *heldEndSub) EndSandbox(context.Context, string) error { s.ends++; return s.endErr }

// A held end is made again as an end, never as a kill, which would delete the sandbox an end keeps. The
// session reaches READY with the hold in place, and the hold is in the saved state: a restarted runner
// still makes the call again until it finishes.
func TestAHeldEndSurvivesARestartAndIsMadeAgainAsAnEnd(t *testing.T) {
	m, local, _, org := managerFixture(t)
	sub := &heldEndSub{localSub: local, endErr: &runner.TeardownUnconfirmedError{Err: errors.New("docker: the macOS VM helper is still running")}}
	m.sub = sub
	held := createLocal(t, m, org)
	m.Offline(t.Context())
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	peer, receipts := receiptPeer(ctx, t)
	end := pendingContainment(m, held, types.RunnerActionEnd)
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{end}); err != nil {
		t.Fatal(err)
	}
	if got := <-receipts; got.ID != end.ID || got.Outcome != types.RunnerActionRefused {
		t.Fatalf("receipt %+v, want the end refused", got)
	}
	if err := m.BeforeReady(ctx, peer, false); err != nil {
		t.Fatalf("with the end held the session did not reach READY: %v", err)
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = again.Close() })
	if h := again.state.Runs[held].TeardownUnconfirmed; h == nil || h.Kind != types.RunnerActionEnd {
		t.Fatalf("after a restart the hold = %+v, want the end held", h)
	}
	if err := again.Pending(ctx, peer, nil); err != nil {
		t.Fatal(err)
	}
	sub.endErr = nil
	if err := again.Pending(ctx, peer, nil); err != nil {
		t.Fatal(err)
	}
	if h := again.state.Runs[held].TeardownUnconfirmed; h != nil || sub.ends != 3 || local.kills != 0 {
		t.Fatalf("hold %+v, %d ends, %d kills; want none, 3 (the action and two sessions) and 0", h, sub.ends, local.kills)
	}
}

// A kill of a held run that finishes drops the hold itself, with no retry between.
func TestAKillOfAHeldRunThatFinishesDropsTheHold(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	held := createLocal(t, m, org)
	sub.killErr = &runner.TeardownUnconfirmedError{Err: errors.New("docker: the macOS VM helper is still running")}
	if res, err := m.apply(t.Context(), pendingContainment(m, held, types.RunnerActionKill)); err != nil || res.Outcome != types.RunnerActionRefused {
		t.Fatalf("held kill = %+v, %v; want refused", res, err)
	}
	if m.state.Runs[held].TeardownUnconfirmed == nil {
		t.Fatal("the kill was not held")
	}
	sub.killErr = nil
	if res, err := m.apply(t.Context(), pendingContainment(m, held, types.RunnerActionKill)); err != nil || res.Outcome == types.RunnerActionRefused {
		t.Fatalf("the kill sent again = %+v, %v; want it applied", res, err)
	}
	if h := m.state.Runs[held].TeardownUnconfirmed; h != nil {
		t.Fatalf("hold %+v after the kill finished, want none", h)
	}
}
