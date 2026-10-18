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
	"github.com/cjohnstoniv/wardyn/internal/store"
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
	s := &hubSession{id: "session", peer: peer, link: link}
	id := s.id
	s.readyID.Store(&id)
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
				s.readyID.Store(nil)
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

// A CURRENT_ACTIVITY the org refuses (the run already left RUNNING) or cannot
// record (a store error) is dropped; the runner's session stays online.
func TestCurrentActivityRefusalKeepsPeerOnline(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	h, s, row, _ := activityFixture(t)
	results := []error{store.ErrRunnerExecutionRefused, errors.New("store unavailable"), nil}
	called := make(chan int, len(results))
	calls := 0
	a := Admission{Activity: func(context.Context, uuid.UUID) error {
		err := results[calls]
		calls++
		called <- calls
		return err
	}}
	var org *runnerwire.Peer
	cfg := runnerwire.PeerConfig{OnControlChecked: func(runnerwire.Frame) error { return nil }}
	h.configureActivity(ctx, &cfg, row, a, s, func() *runnerwire.Peer { return org })
	orgConn, runnerConn := runnerwire.Loopback()
	t.Cleanup(func() { _ = runnerConn.Close() })
	s.mu.Lock()
	org = s.link.AttachSession(orgConn, cfg)
	s.peer = org
	s.mu.Unlock()
	if err := s.link.Ready(org); err != nil {
		t.Fatal(err)
	}
	local := runnerwire.NewPeer(runnerConn, runnerwire.PeerConfig{})
	go org.Run(ctx)   //nolint:errcheck // ends with the test context
	go local.Run(ctx) //nolint:errcheck // ends with the test context
	for want := range len(results) {
		if err := local.CurrentActivity(ctx, uuid.New()); err != nil {
			t.Fatalf("session down before activity %d: send %v, org %v", want+1, err, org.Err())
		}
		select {
		case got := <-called:
			if got != want+1 {
				t.Fatalf("activity call %d, want %d", got, want+1)
			}
		case <-org.Done():
			t.Fatalf("dropped activity failed the session: %v", org.Err())
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if !org.Online() {
		t.Fatalf("session went down: %v", org.Err())
	}
	if err := h.currentActivity(ctx, row, a, &hubSession{id: "old", link: s.link}, org, uuid.New()); !errors.Is(err, runnerwire.ErrNotReady) {
		t.Fatalf("stale session activity not refused: %v", err)
	}
}

// An organization's runner (owner empty) passes the activity barrier; a changed kind or a named owner on it
// is a changed identity.
func TestCurrentActivityAdmitsOrgRunner(t *testing.T) {
	for _, mode := range []string{"valid", "named owner", "changed kind", "revoked"} {
		t.Run(mode, func(t *testing.T) {
			h, s, row, peer := activityFixture(t)
			row.Owner, row.OwnerKind = "", types.RunnerOwnerOrg
			current := row
			switch mode {
			case "named owner":
				current.Owner = "owner"
			case "changed kind":
				current.OwnerKind = types.RunnerOwnerPerson
			case "revoked":
				current.State = types.RunnerRevoked
			}
			h.cfg.Lookup = func(context.Context, uuid.UUID) (types.Runner, error) { return current, nil }
			calls := 0
			a := Admission{Activity: func(context.Context, uuid.UUID) error { calls++; return nil }}
			err := h.currentActivity(t.Context(), row, a, s, peer, uuid.New())
			if (mode == "valid") != (err == nil && calls == 1) {
				t.Fatalf("org runner activity: calls=%d err=%v", calls, err)
			}
		})
	}
}
