// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package offline

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/google/uuid"
)

func TestMissingOutputRetirementDefersWithoutStrandingPending(t *testing.T) {
	m, _, id, spool, action := retirementFixture(t)
	other := createLocal(t, m, m.state.Runs[id].Lease.TokenRenewedAt)
	if err := spool.Append(t.Context(), egress.DecisionLog{Request: egress.Request{RunID: id}}); err != nil {
		t.Fatal(err)
	}
	m.outputSettled = func(context.Context, uuid.UUID, string) (int64, bool, error) {
		return 0, false, runner.ErrOutputUnrecoverable
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	accepted := make(chan runnerwire.ActionResult, 2)
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
	m.Offline(ctx)
	quarantine := quarantineAction(m, id)
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{action, quarantine}); err != nil {
		t.Fatalf("mandatory Pending poisoned by missing output: %v", err)
	}
	first, second := <-accepted, <-accepted
	if first.ID != action.ID || first.Outcome != "deferred" || second.ID != quarantine.ID || second.Outcome != "success" {
		t.Fatalf("pending outcomes: %+v %+v", first, second)
	}
	rows, err := allState(ctx, m)
	if err != nil || len(rows) != 1 || rows[0].RunID != other {
		t.Fatalf("unrelated STATE stranded: %+v %v", rows, err)
	}
	if err := m.Replay(ctx, peer); err != nil {
		t.Fatal(err)
	}
	pos, err := spool.Get(ctx)
	depth, _, _, statsErr := spool.Stats(ctx)
	if err != nil || statsErr != nil || pos.Seq != 0 || depth != 1 || m.state.Runs[id].Retired {
		t.Fatal("unrecoverable output falsely settled/dropped audit")
	}
	if replay, err := m.apply(ctx, action); err != nil || replay != first {
		t.Fatal("deferred receipt was not durable/replayable after quarantine")
	}
}
