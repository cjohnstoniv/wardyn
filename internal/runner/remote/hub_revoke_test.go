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
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestHubActiveRevocationStopsDispatchAndReconnect(t *testing.T) {
	t.Run("revoked here", func(t *testing.T) { testHubRevocation(t, false) })
	t.Run("revoked on another replica", func(t *testing.T) { testHubRevocation(t, true) })
}

// testHubRevocation revokes the runner's stored row and either tells this Hub
// (the replica that committed it) or tells it nothing, as when the revocation
// committed on another replica: the session's own re-read must then end it.
func testHubRevocation(t *testing.T, elsewhere bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	var stored atomic.Pointer[types.Runner]
	stored.Store(&row)
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return *stored.Load(), nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	if elsewhere {
		hub.recheck = 20 * time.Millisecond
	}
	online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
	hooks := sessionHooks(online, resumed)
	var revoked, dials atomic.Int32
	hooks.Revoked = func(context.Context) error { revoked.Add(1); return nil }
	hubDone := make(chan error, 1)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			dials.Add(1)
			org, run := runnerwire.Loopback()
			go func() { hubDone <- hub.Serve(ctx, org, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	awaitValue(t, ctx, online)
	waitOnline(t, ctx, link)
	terminal := row
	terminal.State = types.RunnerRevoked
	stored.Store(&terminal)
	if !elsewhere {
		if err := hub.Revoke(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := awaitValue(t, ctx, done); !errors.Is(err, runnerwire.ErrRevoked) {
		t.Fatalf("revocation result: %v", err)
	}
	awaitValue(t, ctx, hubDone)
	if revoked.Load() != 1 || dials.Load() != 1 || link.Online() || hub.Online(row.ID) {
		t.Fatalf("revoked=%d dials=%d online=%v hub online=%v", revoked.Load(), dials.Load(), link.Online(), hub.Online(row.ID))
	}
	if err := link.Call(ctx, "probe", nil, nil); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("revoked dispatch: %v", err)
	}
}
