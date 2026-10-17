// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestPendingStopProxyOnALostNodeStillReachesReady: a queued stop_proxy whose node is lost answers
// containment pending. A fresh session refuses that action with a receipt and holds the run pending,
// so it still reaches READY and its state page reports the hold the org needs for a node record.
func TestPendingStopProxyOnALostNodeStillReachesReady(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	m.Offline(t.Context())
	sub.stopErr = &runner.ContainmentPendingError{Kind: runner.ContainmentPendingDelete, NameReused: true, Err: errors.New("termination unknown")}

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	receipts := make(chan runnerwire.ActionResult, 1)
	orgConn, runConn := runnerwire.Loopback()
	orgPeer := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnEventChecked: func(event runnerwire.Event) error {
		var result runnerwire.ActionResult
		if err := json.Unmarshal(event.Data, &result); err != nil {
			return err
		}
		receipts <- result
		return nil
	}})
	peer := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	orgPeer.Start(ctx, 0)
	peer.Start(ctx, 0)
	defer orgPeer.Close()
	defer peer.Close()

	a := pendingContainment(m, id, types.RunnerActionStopProxy)
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{a}); err != nil {
		t.Fatalf("a pending stop_proxy failed the session before READY: %v", err)
	}
	if got := <-receipts; got.ID != a.ID || got.Outcome != types.RunnerActionRefused || got.Reason == "" {
		t.Fatalf("receipt %+v, want a refused receipt for %s", got, a.ID)
	}
	if err := m.BeforeReady(ctx, peer, false); err != nil {
		t.Fatalf("session did not reach READY: %v", err)
	}
	if r := m.state.Runs[id]; r.ProxyStoppedAt != nil {
		t.Fatalf("a pending stop was stamped stopped: %+v", r)
	}
	row, err := m.observeRun(ctx, id)
	if err != nil || row == nil || row.ContainmentPending == nil || row.ContainmentPending.Kind != runner.ContainmentPendingDelete || !row.ContainmentPending.NameReused {
		t.Fatalf("state page lacks the pending hold: %+v %v", row, err)
	}
}

// A run the tick already holds pending whose queued stop_proxy then succeeds on a fresh session (the
// node returned while the runner was down) drops the pending report with the stop stamp. A page
// carrying both is refused by the org, so the runner would never reach READY.
func TestApplySuccessClearsPendingBesideStopStamp(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	id := createLocal(t, m, org)
	m.Offline(t.Context())
	if err := m.holdPending(id, &runner.ContainmentPendingError{Kind: runner.ContainmentPendingDelete, Err: errors.New("termination unknown")}); err != nil {
		t.Fatal(err)
	}
	sub.stopErr = nil // the node is back, the pods are gone: StopProxy succeeds
	a := pendingContainment(m, id, types.RunnerActionStopProxy)
	res, err := m.apply(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	r := m.state.Runs[id]
	if r.ProxyStoppedAt == nil {
		t.Fatalf("stop not stamped: %+v", res)
	}
	afterApply := r.ContainmentPending
	if err := m.CheckLapses(t.Context()); err != nil {
		t.Fatal(err)
	}
	afterTick := m.state.Runs[id].ContainmentPending
	if afterApply != nil || afterTick != nil {
		t.Fatalf("pending report kept beside the stop stamp: after apply %+v, after the next tick %+v", afterApply, afterTick)
	}
}
