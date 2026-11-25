// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// streamCatcher keeps every stream the runner opens, unread.
type streamCatcher chan *runnerwire.Stream

func (streamCatcher) HandleEvent(runnerwire.Event) {}

func (c streamCatcher) HandleOpen(s *runnerwire.Stream, _ runnerwire.Open) error {
	c <- s
	return nil
}

// megabyteWrites are one 1 MiB write each way, each to a stream whose far side has not read yet.
type megabyteWrites struct {
	done    [2]chan error
	readers [2]*runnerwire.Stream
}

const megabyte = 1 << 20

// writeMegabyteBothWays opens a relay stream from the org to the runner and one from the runner to the org,
// and starts a 1 MiB write on each.
func writeMegabyteBothWays(t *testing.T, ctx context.Context, org, run *runnerwire.Peer, atRunner, atOrg <-chan *runnerwire.Stream) megabyteWrites {
	t.Helper()
	var w megabyteWrites
	for i, side := range []struct {
		from *runnerwire.Peer
		at   <-chan *runnerwire.Stream
	}{{org, atRunner}, {run, atOrg}} {
		s, err := side.from.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
		if err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
		w.readers[i] = awaitValue(t, ctx, side.at)
		done := make(chan error, 1)
		w.done[i] = done
		go func() { _, err := s.Write(make([]byte, megabyte)); done <- err }()
	}
	return w
}

// completeWithin fails the test unless both writes finish within limit.
func (w megabyteWrites) completeWithin(t *testing.T, limit time.Duration, what string) {
	t.Helper()
	timeout := time.After(limit)
	for i, done := range w.done {
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s: write %d: %v", what, i, err)
			}
		case <-timeout:
			t.Fatalf("%s: the 1 MiB write %d did not complete within %s", what, i, limit)
		}
	}
}

// stopAtTheLegacyWindow checks neither write completes within limit (each stops at StreamWindow), then
// drains both readers and checks the writes complete.
func (w megabyteWrites) stopAtTheLegacyWindow(t *testing.T, limit time.Duration) {
	t.Helper()
	<-time.After(limit)
	for i, done := range w.done {
		select {
		case err := <-done:
			t.Fatalf("the 1 MiB write %d completed on the legacy window (%v)", i, err)
		default:
		}
	}
	for _, r := range w.readers {
		go func() { _, _ = io.ReadFull(r, make([]byte, megabyte)) }()
	}
	w.completeWithin(t, limit, "after the reader drained")
}

// The hub grants a runner's offered window, so a 1 MiB write fits one window both ways; a runner that
// offers none, and a current runner against an organisation that answers none, keep the legacy window.
func TestHubGrantsTheRunnersOfferedWindow(t *testing.T) {
	eachTransport(t, func(t *testing.T, realWire bool) {
		t.Run("no offer", func(t *testing.T) {
			f := newInventoryFixture(t, realWire)
			if _, ok, _ := f.open(t, nil); ok.StreamWindow != 0 {
				t.Fatalf("AUTH_OK granted %d to a runner that offered none", ok.StreamWindow)
			}
		})
		t.Run("negotiated", func(t *testing.T) {
			f := newInventoryFixture(t, realWire)
			atOrg := make(streamCatcher, 1)
			f.link.SetHandler(atOrg)
			online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
			atRunner := make(chan *runnerwire.Stream, 1)
			hooks := sessionHooks(online, resumed)
			hooks.Open = func(_ *runnerwire.Peer, s *runnerwire.Stream, _ runnerwire.Open) error { atRunner <- s; return nil }
			client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: f.orgURL, RunnerID: f.row.ID.String(), PrivateKey: f.key, Hooks: hooks,
				Dial: func(context.Context) (runnerwire.Conn, error) { return f.dial(), nil }})
			if err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- client.Connect(f.ctx) }()
			run := awaitValue(t, f.ctx, online)
			waitOnline(t, f.ctx, f.link)
			writeMegabyteBothWays(t, f.ctx, f.link.ReadyPeer(), run, atRunner, atOrg).completeWithin(t, 2*time.Second, "negotiated")
			run.Close()
			awaitValue(t, f.ctx, done)
		})
		t.Run("older runner", func(t *testing.T) {
			f := newInventoryFixture(t, realWire)
			atOrg := make(streamCatcher, 1)
			f.link.SetHandler(atOrg)
			conn, ok, seq := f.open(t, nil)
			atRunner := make(chan *runnerwire.Stream, 1)
			run := runnerwire.NewPeer(conn, runnerwire.PeerConfig{LastReceived: seq,
				OnOpen: func(s *runnerwire.Stream, _ runnerwire.Open) error { atRunner <- s; return nil }})
			t.Cleanup(run.Close)
			if err := run.Start(f.ctx, ok.LastSeq); err != nil {
				t.Fatal(err)
			}
			for _, fr := range []runnerwire.Frame{{Type: runnerwire.TypeState, Payload: emptyStatePage(t)}, {Type: runnerwire.TypeReady}} {
				if err := run.Send(f.ctx, fr); err != nil {
					t.Fatal(err)
				}
			}
			waitOnline(t, f.ctx, f.link)
			writeMegabyteBothWays(t, f.ctx, f.link.ReadyPeer(), run, atRunner, atOrg).stopAtTheLegacyWindow(t, 2*time.Second)
		})
	})
	t.Run("older organisation", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		pub, key, _ := ed25519.GenerateKey(rand.Reader)
		const orgURL = "https://org.example"
		row := types.Runner{ID: uuid.New(), PublicKey: pub, OrgURLSHA256: federation.OrgURLSHA256(orgURL), State: types.RunnerClaimed}
		orgConn, runConn := runnerwire.Loopback()
		atOrg := make(streamCatcher, 1)
		orgPeer := make(chan *runnerwire.Peer, 1)
		go func() {
			defer orgConn.Close()
			if _, err := runnerwire.AuthenticateRunner(ctx, orgConn, row, orgURL); err != nil {
				return
			}
			// An older organisation's AUTH_OK carries no window.
			ok := runnerwire.AuthOK{SessionID: uuid.NewString(), PingInterval: runnerwire.PingInterval}
			if runnerwire.WriteControl(ctx, orgConn, runnerwire.TypeAuthOK, ok) != nil {
				return
			}
			peer := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnOpen: atOrg.HandleOpen})
			if peer.Start(ctx, 0) != nil {
				return
			}
			defer peer.Close()
			if payload, err := runnerwire.Marshal([]runnerwire.PendingAction{}); err == nil {
				_ = peer.Send(ctx, runnerwire.Frame{Type: runnerwire.TypePending, Payload: payload})
			}
			orgPeer <- peer
			<-ctx.Done()
		}()
		online, resumed := make(chan *runnerwire.Peer, 1), make(chan bool, 1)
		atRunner := make(chan *runnerwire.Stream, 1)
		hooks := sessionHooks(online, resumed)
		hooks.Open = func(_ *runnerwire.Peer, s *runnerwire.Stream, _ runnerwire.Open) error { atRunner <- s; return nil }
		client, err := runnerwire.NewClient(runnerwire.ClientConfig{OrgURL: orgURL, RunnerID: row.ID.String(), PrivateKey: key, Hooks: hooks,
			Dial: func(context.Context) (runnerwire.Conn, error) { return runConn, nil }})
		if err != nil {
			t.Fatal(err)
		}
		done := make(chan error, 1)
		go func() { done <- client.Connect(ctx) }()
		run := awaitValue(t, ctx, online)
		org := awaitValue(t, ctx, orgPeer)
		writeMegabyteBothWays(t, ctx, org, run, atRunner, atOrg).stopAtTheLegacyWindow(t, 2*time.Second)
		cancel()
		<-done
	})
}
