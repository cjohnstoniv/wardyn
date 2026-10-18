// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// A retired run keeps its full record until the org acknowledges the retirement receipt, then shrinks
// to an identity tombstone with no applied actions, which still refuses the run's id after a restart.
func TestRetiredRunCompactsToATombstoneOnceTheOrgAcknowledges(t *testing.T) {
	m, sub, id, _, a := retirementFixture(t)
	got, err := m.apply(t.Context(), a)
	if err != nil || got.Outcome != "success" {
		t.Fatalf("retire: %+v %v", got, err)
	}
	if r := m.state.Runs[id]; r.Retirement == nil || r.Tombstone {
		t.Fatal("compacted before the org acknowledged the receipt")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	accepted := make(chan runnerwire.ActionResult, 1)
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
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{a}); err != nil {
		t.Fatal(err)
	}
	if res := <-accepted; res.ID != got.ID || res.Outcome != got.Outcome {
		t.Fatalf("org got %+v, want the saved receipt %+v", res, got)
	}
	if r := m.state.Runs[id]; !r.Tombstone || !r.Retired || r.Retirement != nil || r.Containment != nil || r.Ref != id.String() {
		t.Fatalf("retired run not compacted: %+v", r)
	}
	for _, x := range m.state.Actions {
		if x.Action.RunID == id {
			t.Fatalf("applied action %s kept for a tombstoned run", x.Action.ID)
		}
	}
	if err := m.BeforeCreate(t.Context(), runner.SandboxSpec{RunID: id}); err == nil {
		t.Fatal("tombstoned run id reused")
	}
	if err := m.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := New(m.dir, m.runnerID, sub, uuid.UUID.String, noOutput, noErase)
	if err != nil {
		t.Fatalf("a tombstone does not load: %v", err)
	}
	defer reopened.Close()
	if res, err := reopened.apply(t.Context(), pendingContainment(reopened, id, types.RunnerActionKill)); err != nil || res.Outcome != types.RunnerActionRefused {
		t.Fatalf("an action for a tombstoned run: %+v %v, want refused", res, err)
	}
}
