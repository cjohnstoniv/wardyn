// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func activityFixture(t *testing.T) (*Hub, *hubSession, types.Runner, *runnerwire.Peer) {
	t.Helper()
	row := types.Runner{ID: uuid.New(), Owner: "owner", State: types.RunnerClaimed, PublicKey: []byte("key"), OrgURLSHA256: "org"}
	hub := NewHub(t.Context(), HubConfig{Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil }})
	t.Cleanup(hub.Close)
	org, local := runnerwire.Loopback()
	t.Cleanup(func() { _ = local.Close() })
	link := &Link{}
	peer := link.AttachSession(org, runnerwire.PeerConfig{})
	if err := link.Ready(peer); err != nil {
		t.Fatal(err)
	}
	s := &hubSession{peer: peer, link: link}
	s.ready.Store(true)
	hub.sessions[row.ID] = s
	return hub, s, row, peer
}

func TestCurrentActivityRejectsStaleSessionAndChangedIdentity(t *testing.T) {
	for _, mode := range []string{"valid", "not ready", "old peer", "expired session", "closed hub", "unclaimed", "revoked", "changed owner", "empty owner", "changed key", "changed org", "wrong runner", "lookup error", "no callback"} {
		t.Run(mode, func(t *testing.T) {
			h, s, row, peer := activityFixture(t)
			current := row
			switch mode {
			case "not ready":
				s.ready.Store(false)
			case "old peer":
				s.peer = &runnerwire.Peer{}
			case "expired session":
				delete(h.sessions, row.ID)
			case "closed hub":
				h.closed = true
			case "unclaimed":
				current.State = types.RunnerUnclaimed
			case "revoked":
				current.State = types.RunnerRevoked
			case "changed owner":
				current.Owner = "another"
			case "empty owner":
				current.Owner = ""
			case "changed key":
				current.PublicKey = []byte("another")
			case "changed org":
				current.OrgURLSHA256 = "another"
			case "wrong runner":
				current.ID = uuid.New()
			}
			h.cfg.Lookup = func(context.Context, uuid.UUID) (types.Runner, error) {
				if mode == "lookup error" {
					return types.Runner{}, errors.New("store unavailable")
				}
				return current, nil
			}
			run, calls := uuid.New(), 0
			a := Admission{Activity: func(_ context.Context, id uuid.UUID) error {
				calls++
				if id != run {
					t.Error("activity changed run")
				}
				return nil
			}}
			if mode == "no callback" {
				a.Activity = nil
			}
			err := h.currentActivity(t.Context(), row, a, s, peer, run)
			if mode == "valid" {
				if err != nil || calls != 1 {
					t.Fatalf("current activity refused: calls=%d err=%v", calls, err)
				}
			} else if err == nil || calls != 0 {
				t.Fatalf("stale activity reached callback: calls=%d err=%v", calls, err)
			}
		})
	}
}

func TestCurrentActivityHoldsReplacementAndRevocationBarrier(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, s, row, peer := activityFixture(t)
	entered, release := make(chan struct{}), make(chan struct{})
	a := Admission{Activity: func(context.Context, uuid.UUID) error {
		close(entered)
		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}}
	done := make(chan error, 1)
	go func() { done <- h.currentActivity(ctx, row, a, s, peer, uuid.New()) }()
	awaitValue(t, ctx, entered)
	if s.mu.TryLock() {
		s.mu.Unlock()
		t.Fatal("activity callback ran outside the connection replacement barrier")
	}
	revoked := make(chan error, 1)
	go func() { revoked <- h.Revoke(ctx, row.ID) }()
	close(release)
	if err := awaitValue(t, ctx, done); err != nil {
		t.Fatal(err)
	}
	if err := awaitValue(t, ctx, revoked); err != nil {
		t.Fatal(err)
	}
	if err := h.currentActivity(ctx, row, a, s, peer, uuid.New()); !errors.Is(err, runnerwire.ErrNotReady) {
		t.Fatal("revoked peer admitted current activity", err)
	}
}

func TestCurrentActivityControlCannotBeHistoricalEvent(t *testing.T) {
	h, s, row, peer := activityFixture(t)
	calls := 0
	a := Admission{Event: func(context.Context, runnerwire.Event) error { return nil }, Activity: func(context.Context, uuid.UUID) error { calls++; return nil }}
	cfg := runnerwire.PeerConfig{}
	configureAdmission(t.Context(), &cfg, true, nil, a, s, "", func() *runnerwire.Peer { return peer })
	h.configureActivity(t.Context(), &cfg, row, a, s, func() *runnerwire.Peer { return peer })
	for _, kind := range []string{runnerwire.EventSpool, runnerwire.EventAgentExit} {
		if err := cfg.OnEventChecked(runnerwire.Event{Kind: kind}); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 0 {
		t.Fatal("historical event touched current activity")
	}
	for _, frame := range []runnerwire.Frame{
		{Type: runnerwire.TypeCurrentActivity, Payload: []byte(`{}`)},
		{Type: runnerwire.TypeCurrentActivity, Payload: []byte(`null`)},
		{Type: runnerwire.TypeCurrentActivity, Payload: []byte(`{"run_id":"not-a-uuid"}`)},
	} {
		if err := cfg.OnControlChecked(frame); err == nil {
			t.Fatal("invalid activity accepted")
		}
	}
	if calls != 0 {
		t.Fatal("invalid control touched activity")
	}
	payload, _ := runnerwire.Marshal(runnerwire.CurrentActivity{RunID: uuid.New()})
	if err := cfg.OnControlChecked(runnerwire.Frame{Type: runnerwire.TypeCurrentActivity, Payload: payload}); err != nil || calls != 1 {
		t.Fatalf("valid control calls=%d err=%v", calls, err)
	}
}
