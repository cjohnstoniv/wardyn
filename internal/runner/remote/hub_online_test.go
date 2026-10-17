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
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Online is the session's own READY on a live peer: not a connected peer that
// has not reached READY, not a session in transition, and not after GoawayAll.
func TestHubOnlineIsReadyOnALivePeerAndGoawayAllEndsIt(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	var refuse atomic.Bool
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			if refuse.Load() {
				return Admission{}, errors.New("admission closed")
			}
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	defer hub.Close()
	online, resumed := make(chan *runnerwire.Peer, 4), make(chan bool, 4)
	hooks := sessionHooks(online, resumed)
	gate, entered := make(chan struct{}), make(chan struct{}, 1)
	hooks.BeforeReady = func(context.Context, *runnerwire.Peer, bool) error {
		entered <- struct{}{}
		<-gate
		return nil
	}
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- client.Connect(ctx) }()
	awaitValue(t, ctx, entered)
	if hub.Online(row.ID) {
		t.Fatal("a connected session reads online before its READY")
	}
	close(gate)
	waitOnline(t, ctx, link)
	if !hub.Online(row.ID) || hub.Online(uuid.New()) {
		t.Fatalf("online(runner)=%v online(other)=%v", hub.Online(row.ID), hub.Online(uuid.New()))
	}
	hub.mu.Lock()
	s := hub.sessions[row.ID]
	hub.mu.Unlock()
	s.mu.Lock()
	transition := hub.Online(row.ID)
	s.mu.Unlock()
	if transition {
		t.Fatal("a session in transition reads online")
	}
	refuse.Store(true)
	if err := hub.GoawayAll(ctx); err != nil {
		t.Fatal(err)
	}
	awaitValue(t, ctx, done)
	if hub.Online(row.ID) || link.Online() {
		t.Fatal("a runner reads online after GoawayAll")
	}
}

func TestSubstrateDispatchGateRefusesCreateBeforeTheWire(t *testing.T) {
	closed := errors.New("dispatch closed")
	var calls atomic.Int32
	sub := New(uuid.NewString(), &Link{}, Options{Dispatch: func(context.Context) error { calls.Add(1); return closed }})
	if _, err := sub.CreateSandbox(t.Context(), runner.SandboxSpec{RunID: uuid.New(), ConfinementClass: types.CC1}); !errors.Is(err, closed) || calls.Load() != 1 {
		t.Fatalf("create = %v after %d gate reads, want the gate's refusal after one", err, calls.Load())
	}
}

// A Connected callback that fails (the runner row could not be read for the audit) refuses the
// session: Serve returns that error and the runner never reads online.
func TestHubConnectedFailureRefusesTheSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	errRead := errors.New("read runner")
	var disconnected atomic.Bool
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Connected:    func(context.Context, uuid.UUID) error { return errRead },
		Disconnected: func(context.Context, uuid.UUID) error { disconnected.Store(true); return nil },
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
	served := make(chan error, 4)
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key,
		Hooks: sessionHooks(make(chan *runnerwire.Peer, 4), make(chan bool, 4)),
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { served <- hub.Serve(ctx, org, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = client.Connect(ctx) }()
	if err := awaitValue(t, ctx, served); !errors.Is(err, errRead) {
		t.Fatalf("Serve = %v, want the Connected error", err)
	}
	if hub.Online(row.ID) || link.Online() || disconnected.Load() {
		t.Fatalf("a refused session reads online=%v link=%v or reported Disconnected=%v", hub.Online(row.ID), link.Online(), disconnected.Load())
	}
}

// Closing the hub at daemon shutdown reports Disconnected for a session that reported Connected.
func TestHubCloseReportsDisconnectedForAConnectedSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}
	link := &Link{}
	sub := New(row.ID.String(), link, Options{})
	var connected, disconnected atomic.Int32
	hub := NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return row, nil },
		Connected:    func(context.Context, uuid.UUID) error { connected.Add(1); return nil },
		Disconnected: func(context.Context, uuid.UUID) error { disconnected.Add(1); return nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(context.Context, string) error { return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
	})
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key,
		Hooks: sessionHooks(make(chan *runnerwire.Peer, 4), make(chan bool, 4)),
		Dial: func(context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			go func() { _ = hub.Serve(ctx, org, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = client.Connect(ctx) }()
	waitOnline(t, ctx, link)
	hub.Close()
	hub.Close() // a second Close reports nothing more
	if connected.Load() != 1 || disconnected.Load() != 1 {
		t.Fatalf("connected %d, disconnected %d; want 1 and 1", connected.Load(), disconnected.Load())
	}
}
