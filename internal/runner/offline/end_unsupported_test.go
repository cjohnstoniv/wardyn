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
)

// A queued end on a substrate that cannot keep an ended sandbox (a Kubernetes runner) is refused with a
// receipt, not failed: a failed Pending ends every fresh session before READY, so the runner never came
// back. The session goes on to apply the actions queued after it, a replay answers the same receipt,
// and no containment marker is begun for the refused end.
func TestPendingEndOnASubstrateThatCannotKeepIsRefused(t *testing.T) {
	m, sub, _, org := managerFixture(t)
	ended, killed := createLocal(t, m, org), createLocal(t, m, org)
	m.Offline(t.Context())
	before := m.state.Runs[ended].Containment

	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	receipts := make(chan runnerwire.ActionResult, 3)
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

	end, kill := pendingContainment(m, ended, types.RunnerActionEnd), pendingContainment(m, killed, types.RunnerActionKill)
	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{end, kill}); err != nil {
		t.Fatalf("a queued end failed the session before READY: %v", err)
	}
	refused := <-receipts
	if refused.ID != end.ID || refused.Outcome != types.RunnerActionRefused || refused.Reason != runner.ErrEndUnsupported.Error() {
		t.Fatalf("end receipt %+v, want refused with %q", refused, runner.ErrEndUnsupported)
	}
	if got := <-receipts; got.ID != kill.ID || got.Outcome == types.RunnerActionRefused {
		t.Fatalf("kill receipt %+v, want the kill queued after the end applied", got)
	}
	if sub.kills != 1 {
		t.Fatalf("KillSandbox ran %d times, want 1", sub.kills)
	}
	if got := m.state.Runs[ended].Containment; got != before {
		t.Fatalf("the refused end began containment: %+v", got)
	}

	if err := m.Pending(ctx, peer, []runnerwire.PendingAction{end}); err != nil {
		t.Fatalf("a replayed end failed the session: %v", err)
	}
	if got := <-receipts; got.ID != refused.ID || got.Outcome != refused.Outcome || got.Reason != refused.Reason || !got.ObservedAt.Equal(refused.ObservedAt) {
		t.Fatalf("replayed receipt %+v, want the saved %+v", got, refused)
	}
	if err := m.BeforeReady(ctx, peer, false); err != nil {
		t.Fatalf("session did not reach READY: %v", err)
	}
}
