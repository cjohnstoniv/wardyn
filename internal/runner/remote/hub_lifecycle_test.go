// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestHubClosedCannotAllocateSession(t *testing.T) {
	hub := NewHub(context.Background(), HubConfig{})
	id := uuid.New()
	session, err := hub.lockSession(id)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := hub.lockSession(id); err == nil {
		t.Fatal("parallel session transition acquired the same slot")
	}
	session.mu.Unlock()
	hub.Close()
	if _, err := hub.lockSession(id); !errors.Is(err, runnerwire.ErrLinkDown) {
		t.Fatalf("closed hub admitted a new session: %v", err)
	}
}

func TestHubGoawayForcesFreshAdmissionAndResetsBytes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	var states atomic.Int32
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { states.Add(1); return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 2), make(chan bool, 2)
	opened := make(chan *runnerwire.Stream, 1)
	hooks := sessionHooks(online, resumed)
	hooks.Open = func(_ *runnerwire.Peer, stream *runnerwire.Stream, _ runnerwire.Open) error {
		opened <- stream
		return nil
	}
	hubDone := make(chan error, 2)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { hubDone <- hub.Serve(ctx, org, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 2)
	go func() { done <- client.Connect(ctx) }()
	awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) {
		t.Fatal("first connection resumed")
	}
	waitOnline(t, ctx, link)
	stream, err := link.current().Open(ctx, runnerwire.Open{Kind: runnerwire.KindExec})
	if err != nil {
		t.Fatal(err)
	}
	other := awaitValue(t, ctx, opened)
	if err := hub.Goaway(ctx, row.ID); err != nil {
		t.Fatal(err)
	}
	if err := awaitValue(t, ctx, done); !errors.Is(err, runnerwire.ErrGoaway) {
		t.Fatalf("GOAWAY control: %v", err)
	}
	awaitValue(t, ctx, hubDone)
	for _, st := range []*runnerwire.Stream{stream, other} {
		var reset *runnerwire.ResetError
		if _, err := st.Read(make([]byte, 1)); !errors.As(err, &reset) || reset.Code != runnerwire.ResetReconnect {
			t.Fatalf("byte stream survived reconnect: %v", err)
		}
	}
	go func() { done <- client.Connect(ctx) }()
	peer := awaitValue(t, ctx, online)
	if awaitValue(t, ctx, resumed) {
		t.Fatal("GOAWAY resumed the previous session")
	}
	waitOnline(t, ctx, link)
	if states.Load() != 2 || peer.StreamCount() != 0 {
		t.Fatalf("fresh admission/state=%d byte streams=%d", states.Load(), peer.StreamCount())
	}
	peer.Close()
	awaitValue(t, ctx, done)
}
