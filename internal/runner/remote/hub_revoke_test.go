// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
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
	testHubRevocation(t, false)
}

// A revocation committed through another replica reaches this replica's
// session only through its stored-row recheck.
func TestHubStoredRevocationFromAnotherReplicaEndsTheSession(t *testing.T) {
	recheck := storedRowRecheck
	storedRowRecheck = 20 * time.Millisecond
	t.Cleanup(func() { storedRowRecheck = recheck })
	testHubRevocation(t, true)
}

func testHubRevocation(t *testing.T, otherReplica bool) {
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
	if !otherReplica {
		if err := hub.Revoke(ctx, row.ID); err != nil {
			t.Fatal(err)
		}
	}
	if err := awaitValue(t, ctx, done); !errors.Is(err, runnerwire.ErrRevoked) {
		t.Fatalf("revocation result: %v", err)
	}
	awaitValue(t, ctx, hubDone)
	if revoked.Load() != 1 || dials.Load() != 1 || link.Online() {
		t.Fatalf("revoked=%d dials=%d online=%v", revoked.Load(), dials.Load(), link.Online())
	}
	if err := link.Call(ctx, "probe", nil, nil); !errors.Is(err, runner.ErrRunnerOffline) {
		t.Fatalf("revoked dispatch: %v", err)
	}
}

// stallConn is the org end of a link whose runner stopped reading: once
// stalled, a write blocks until its ctx ends or the conn closes.
type stallConn struct {
	runnerwire.Conn
	stalled   atomic.Bool
	blocked   chan struct{}
	block     sync.Once
	closed    chan struct{}
	closeOnce sync.Once
}

func (c *stallConn) WriteFrame(ctx context.Context, f runnerwire.Frame) error {
	if !c.stalled.Load() {
		return c.Conn.WriteFrame(ctx, f)
	}
	c.block.Do(func() { close(c.blocked) })
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.closed:
		return runnerwire.ErrLinkDown
	}
}

func (c *stallConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// R2-0103: Revoke ends a session whose write is stalled within its one-second
// bound, closing the link, instead of waiting behind the stalled write.
func TestHubRevokeEndsASessionWithAStalledWrite(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
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
	online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
	conns := make(chan *stallConn, 1)
	hubDone := make(chan error, 1)
	var dials atomic.Int32
	client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: row.ID.String(), PrivateKey: key, Hooks: sessionHooks(online, resumed),
		Dial: func(context.Context) (runnerwire.Conn, error) {
			if dials.Add(1) > 1 {
				return nil, errors.New("one connection only")
			}
			org, run := runnerwire.Loopback()
			sc := &stallConn{Conn: org, blocked: make(chan struct{}), closed: make(chan struct{})}
			conns <- sc
			go func() { hubDone <- hub.Serve(ctx, sc, row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = client.Run(ctx) }()
	awaitValue(t, ctx, online)
	waitOnline(t, ctx, link)
	sc := awaitValue(t, ctx, conns)
	terminal := row
	terminal.State = types.RunnerRevoked
	stored.Store(&terminal)
	sc.stalled.Store(true)
	go func() { _ = link.Call(ctx, "probe", nil, nil) }()
	awaitValue(t, ctx, sc.blocked) // the CALL holds the write lock in a write that never moves
	revoked := make(chan error, 1)
	go func() { revoked <- hub.Revoke(ctx, row.ID) }()
	select {
	case <-revoked:
	case <-time.After(3 * time.Second):
		_ = sc.Close() // unblock the stalled write so the test can end
		t.Fatal("Revoke waited behind a stalled write past its one-second bound")
	}
	awaitValue(t, ctx, hubDone)
}
