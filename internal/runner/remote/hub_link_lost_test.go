// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type linkLostCall struct {
	runner     uuid.UUID
	session    string
	droppedFor time.Duration
}

// linkLostRig is one hub and one runner over loopback connections, recording each LinkLost call and the id of each
// fresh session. It is built inside a synctest bubble, so the resume timer runs on the bubble's clock.
type linkLostRig struct {
	ctx    context.Context
	cancel context.CancelFunc
	hub    *Hub
	row    types.Runner
	key    ed25519.PrivateKey
	link   *Link
	began  chan string

	mu    sync.Mutex
	lost  []linkLostCall
	conns []*runnerwire.LoopConn
}

func newLinkLostRig(t *testing.T) *linkLostRig {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	r := &linkLostRig{ctx: ctx, cancel: cancel, key: key, link: &Link{}, began: make(chan string, 4),
		row: types.Runner{ID: uuid.New(), PublicKey: pub, State: types.RunnerClaimed, OrgURLSHA256: federation.OrgURLSHA256("https://org.example")}}
	sub := New(r.row.ID.String(), r.link, Options{})
	r.hub = NewHub(ctx, HubConfig{OrgURL: "https://org.example", Lookup: func(context.Context, uuid.UUID) (types.Runner, error) { return r.row, nil },
		Admit: func(context.Context, types.Runner) (Admission, error) {
			return Admission{Substrate: sub, Link: r.link,
				Pending:        func(context.Context) ([]runnerwire.PendingAction, error) { return nil, nil },
				BeginInventory: func(_ context.Context, session string) error { r.began <- session; return nil },
				StatePage:      func(context.Context, string, runnerwire.StatePage) error { return nil },
				Event:          func(context.Context, runnerwire.Event) error { return nil },
			}, nil
		},
		Connected: func(context.Context, uuid.UUID) error { return nil },
		LinkLost: func(_ context.Context, runnerID uuid.UUID, sessionID string, droppedFor time.Duration) {
			r.mu.Lock()
			r.lost = append(r.lost, linkLostCall{runnerID, sessionID, droppedFor})
			r.mu.Unlock()
		},
	})
	return r
}

// client is a runner that dials the hub over a new loopback connection each time it connects.
func (r *linkLostRig) client(t *testing.T, online chan *runnerwire.Peer, resumed chan bool) *runnerwire.Client {
	t.Helper()
	c, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: "https://org.example", RunnerID: r.row.ID.String(), PrivateKey: r.key,
		Hooks: sessionHooks(online, resumed),
		Dial: func(ctx context.Context) (runnerwire.Conn, error) {
			org, run := runnerwire.Loopback()
			r.mu.Lock()
			r.conns = append(r.conns, run)
			r.mu.Unlock()
			go func() { _ = r.hub.Serve(r.ctx, org, r.row.ID) }()
			return run, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// connect runs one connection of c and waits until it is online; it returns the channel its end is sent on.
func (r *linkLostRig) connect(t *testing.T, c *runnerwire.Client, online chan *runnerwire.Peer, resumed chan bool, wantResumed bool) chan error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.Connect(r.ctx) }()
	awaitValue(t, r.ctx, online)
	if got := awaitValue(t, r.ctx, resumed); got != wantResumed {
		t.Fatalf("session resumed = %v, want %v", got, wantResumed)
	}
	waitOnline(t, r.ctx, r.link)
	return done
}

// drop cuts the newest connection and waits for the runner's side to end.
func (r *linkLostRig) drop(t *testing.T, done chan error) {
	t.Helper()
	r.mu.Lock()
	conn := r.conns[len(r.conns)-1]
	r.mu.Unlock()
	conn.Drop()
	awaitValue(t, r.ctx, done)
}

func (r *linkLostRig) calls() []linkLostCall {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]linkLostCall(nil), r.lost...)
}

func (r *linkLostRig) stop() {
	r.cancel()
	r.hub.Close()
	synctest.Wait()
}

// TestHub_LinkLostOnlyOnResumeTimeout: LinkLost fires once, with the ended session's id, when the resume window
// passes with the session not resumed, and never for a resumed session, a fresh session inside the window, or Close.
func TestHub_LinkLostOnlyOnResumeTimeout(t *testing.T) {
	t.Run("a drop not resumed", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newLinkLostRig(t)
			defer r.stop()
			online, resumed := make(chan *runnerwire.Peer, 4), make(chan bool, 4)
			c := r.client(t, online, resumed)
			done := r.connect(t, c, online, resumed, false)
			session := awaitValue(t, r.ctx, r.began)
			r.drop(t, done)
			time.Sleep(runnerwire.ResumeWindow + time.Second)
			synctest.Wait()
			got := r.calls()
			if len(got) != 1 {
				t.Fatalf("LinkLost calls = %d, want 1", len(got))
			}
			if got[0].runner != r.row.ID || got[0].session != session || got[0].droppedFor < runnerwire.ResumeWindow {
				t.Fatalf("LinkLost(%s, %q, %v); want (%s, %q, >= %v)", got[0].runner, got[0].session, got[0].droppedFor,
					r.row.ID, session, runnerwire.ResumeWindow)
			}
		})
	})
	t.Run("a drop resumed inside the window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newLinkLostRig(t)
			defer r.stop()
			online, resumed := make(chan *runnerwire.Peer, 4), make(chan bool, 4)
			c := r.client(t, online, resumed)
			done := r.connect(t, c, online, resumed, false)
			r.drop(t, done)
			time.Sleep(10 * time.Second)
			r.connect(t, c, online, resumed, true)
			time.Sleep(runnerwire.ResumeWindow + time.Second)
			synctest.Wait()
			if got := r.calls(); len(got) != 0 {
				t.Fatalf("LinkLost calls for a resumed session = %+v, want none", got)
			}
		})
	})
	t.Run("a drop, then a fresh session inside the window", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newLinkLostRig(t)
			defer r.stop()
			online, resumed := make(chan *runnerwire.Peer, 4), make(chan bool, 4)
			done := r.connect(t, r.client(t, online, resumed), online, resumed, false)
			r.drop(t, done)
			time.Sleep(10 * time.Second)
			r.connect(t, r.client(t, online, resumed), online, resumed, false) // a new client carries no resume
			time.Sleep(runnerwire.ResumeWindow + time.Second)
			synctest.Wait()
			if got := r.calls(); len(got) != 0 {
				t.Fatalf("LinkLost calls after a fresh session = %+v, want none", got)
			}
		})
	})
	t.Run("Close with a timer pending", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			r := newLinkLostRig(t)
			defer r.stop()
			online, resumed := make(chan *runnerwire.Peer, 4), make(chan bool, 4)
			done := r.connect(t, r.client(t, online, resumed), online, resumed, false)
			r.drop(t, done)
			time.Sleep(10 * time.Second)
			r.hub.Close()
			time.Sleep(runnerwire.ResumeWindow + time.Second)
			synctest.Wait()
			if got := r.calls(); len(got) != 0 {
				t.Fatalf("LinkLost calls after Close = %+v, want none", got)
			}
		})
	})
}
