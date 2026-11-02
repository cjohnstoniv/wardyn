// Copyright 2026 The Wardyn Authors
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

// GoawaySession ends only the fresh session it names. A caller that decided on an older session, and
// runs late, leaves the session that replaced it up, and waits for an admission in progress instead of
// cancelling it.
func TestHubGoawaySessionEndsOnlyThatSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	sessions, gate := make(chan string, 4), make(chan struct{})
	var admissions atomic.Int32
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending: func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(c context.Context, session string) error {
					sessions <- session
					if admissions.Add(1) == 3 { // the third admission stalls, holding the slot
						select {
						case <-gate:
						case <-c.Done():
							return c.Err()
						}
					}
					return nil
				},
				StatePage: func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:     func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 4), make(chan bool, 4)
	hubDone, done := make(chan error, 4), make(chan error, 4)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key,
		Hooks: sessionHooks(online, resumed),
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { hubDone <- hub.Serve(ctx, org, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	connected := func() {
		t.Helper()
		awaitValue(t, ctx, online)
		waitOnline(t, ctx, link)
	}
	end := func(session string) {
		t.Helper()
		if err := hub.GoawaySession(ctx, row.ID, session); err != nil {
			t.Fatal(err)
		}
		if err := awaitValue(t, ctx, done); !errors.Is(err, runnerwire.ErrGoaway) {
			t.Fatalf("ending %s: %v, want a GOAWAY", session, err)
		}
		awaitValue(t, ctx, hubDone)
	}
	stillUp := func(what string) {
		t.Helper()
		select {
		case err := <-done:
			t.Fatalf("%s ended the current session: %v", what, err)
		case <-time.After(200 * time.Millisecond):
		}
		if !hub.Online(row.ID) {
			t.Fatalf("%s: the current session is not online", what)
		}
	}

	go func() { done <- client.Connect(ctx) }()
	s1 := awaitValue(t, ctx, sessions)
	connected()
	end(s1)

	go func() { done <- client.Connect(ctx) }()
	s2 := awaitValue(t, ctx, sessions)
	connected()
	if err := hub.GoawaySession(ctx, row.ID, s1); err != nil { // decided on s1, runs after s2 replaced it
		t.Fatal(err)
	}
	stillUp("a GOAWAY for the replaced session")
	end(s2)

	go func() { done <- client.Connect(ctx) }()
	s3 := awaitValue(t, ctx, sessions) // its admission now holds the slot
	stale := make(chan error, 1)
	go func() { stale <- hub.GoawaySession(ctx, row.ID, s2) }()
	time.Sleep(50 * time.Millisecond) // let it reach the held slot
	close(gate)
	if err := awaitValue(t, ctx, stale); err != nil {
		t.Fatal(err)
	}
	connected()
	stillUp("a GOAWAY for the session an admission was replacing")
	end(s3)
}
