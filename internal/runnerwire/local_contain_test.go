// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const containOrgURL = "https://org.example"

// stoppedPeer is a session that ran and ended, so its client may resume it.
func stoppedPeer(t *testing.T) *Peer {
	t.Helper()
	org, run := Loopback()
	p := NewPeer(run, PeerConfig{})
	org.Close()
	go func() { _ = p.Run(context.Background()) }()
	select {
	case <-p.stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("the peer did not stop on a closed link")
	}
	return p
}

// resumableClient is a client whose last session reached READY and just ended, dialling through dial.
func resumableClient(t *testing.T, hooks RunnerHooks, dial func(context.Context) (Conn, error)) (*Client, types.Runner) {
	t.Helper()
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(containOrgURL), State: types.RunnerClaimed}
	c, err := NewClient(ClientConfig{OrgURL: containOrgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks, Dial: dial})
	if err != nil {
		t.Fatal(err)
	}
	c.peer, c.ready, c.session, c.ended = stoppedPeer(t), true, uuid.NewString(), time.Now()
	return c, row
}

// RequireFresh ends the live session and the next connect asks for no resume, so its STATE page precedes READY.
func TestRequireFresh_NextSessionSendsNoResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	resumes := make(chan *Resume, 1)
	var row types.Runner
	c, row := resumableClient(t, idleHooks(), func(context.Context) (Conn, error) {
		org, run := Loopback()
		go func() {
			defer org.Close()
			auth, err := AuthenticateRunner(ctx, org, row, containOrgURL)
			if err != nil {
				t.Error(err)
			}
			resumes <- auth.Resume
		}()
		return run, nil
	})
	if _, r := c.resume(ctx); r == nil {
		t.Fatal("the client would not resume before RequireFresh; the test proves nothing")
	}
	liveOrg, liveRun := Loopback()
	defer liveOrg.Close()
	live := NewPeer(liveRun, PeerConfig{})
	c.live.Store(live)
	c.RequireFresh()
	select {
	case <-live.Done():
	default:
		t.Fatal("RequireFresh left the live session up")
	}
	_ = c.Connect(ctx)
	select {
	case r := <-resumes:
		if r != nil {
			t.Fatalf("the connect after RequireFresh asked to resume %+v", r)
		}
	case <-ctx.Done():
		t.Fatal("the organisation never saw the connect")
	}
}

// A latch that lands after the client chose to resume, while it authenticates, ends the resumed session before
// BeforeReady and the next connect is fresh.
func TestRequireFresh_LatchWhileResumeIsDialling(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	resumes := make(chan *Resume, 4)
	var clocks, beforeReady atomic.Int32
	var c *Client
	hooks := idleHooks()
	hooks.Clock = func(Hello, time.Time) error {
		if clocks.Add(1) == 1 {
			c.RequireFresh() // the resume is already chosen: authenticateOrg runs after resume()
		}
		return nil
	}
	hooks.BeforeReady = func(context.Context, *Peer, bool) error { beforeReady.Add(1); return nil }
	var row types.Runner
	c, row = resumableClient(t, hooks, func(context.Context) (Conn, error) {
		org, run := Loopback()
		go func() {
			defer org.Close()
			auth, err := AuthenticateRunner(ctx, org, row, containOrgURL)
			if err != nil {
				return
			}
			resumes <- auth.Resume
			if auth.Resume != nil {
				_ = WriteControl(ctx, org, TypeAuthOK, AuthOK{SessionID: auth.Resume.SessionID, Resumed: true, PingInterval: PingInterval})
			}
		}()
		return run, nil
	})
	done := make(chan error, 1)
	go func() { done <- c.Run(ctx) }()
	var got []*Resume
	for len(got) < 2 {
		select {
		case r := <-resumes:
			got = append(got, r)
		case <-ctx.Done():
			t.Fatalf("saw %d connects, want 2", len(got))
		}
	}
	cancel()
	<-done
	if got[0] == nil {
		t.Fatal("the first connect did not resume; the test proves nothing")
	}
	if got[1] != nil {
		t.Fatalf("the connect after the latch asked to resume %+v", got[1])
	}
	if n := beforeReady.Load(); n != 0 {
		t.Fatalf("BeforeReady ran %d times on the resumed session the latch ended", n)
	}
}
