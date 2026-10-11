// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Session A's READY is delivered after session B's fresh admission began but
// before B attached, so A's own Link.Ready still succeeds. Driving the
// callbacks in that order is the race, without timing.
func TestHubLateReadyCannotAdmitNextFreshSession(t *testing.T) {
	ctx := t.Context()
	row := types.Runner{ID: uuid.New(), Owner: "owner", State: types.RunnerClaimed}
	h := NewHub(ctx, HubConfig{Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil }})
	t.Cleanup(h.Close)
	link := &Link{}
	store := newInventoryStore(nil)
	a := store.admission(nil, link)
	activity := 0
	a.Activity = func(context.Context, uuid.UUID) error { activity++; return nil }
	s := &hubSession{link: link}
	h.sessions[row.ID] = s

	if _, err := h.fresh(ctx, s, a, row.ID); err != nil {
		t.Fatal(err)
	}
	var cfgA runnerwire.PeerConfig
	var peerA *runnerwire.Peer
	configureAdmission(ctx, &cfgA, false, nil, a, s, s.id, func() *runnerwire.Peer { return peerA })
	org, run := runnerwire.Loopback()
	t.Cleanup(func() { _ = run.Close() })
	peerA = link.AttachSession(org, runnerwire.PeerConfig{})
	if err := peerA.Start(ctx, 0); err != nil {
		t.Fatal(err)
	}
	s.peer = peerA
	if err := cfgA.OnControlChecked(stateFrame(t, inventoryPages(orderedRuns(1))[0])); err != nil {
		t.Fatal(err)
	}

	if _, err := h.fresh(ctx, s, a, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := cfgA.OnControlChecked(runnerwire.Frame{Type: runnerwire.TypeReady}); err != nil {
		t.Fatal("session A's own READY was refused", err)
	}
	if err := h.currentActivity(ctx, row, a, s, peerA, uuid.New()); !errors.Is(err, runnerwire.ErrNotReady) || activity != 0 {
		t.Errorf("late READY admitted current activity for the next session: %v calls=%d", err, activity)
	}
	peerA.Close()
	s.mu.Lock()
	_, resumed, err := h.resume(ctx, s, &runnerwire.Resume{SessionID: s.id})
	s.mu.Unlock()
	if resumed || err != nil {
		t.Fatalf("late READY made the incomplete next session resumable: resumed=%v err=%v", resumed, err)
	}
}
