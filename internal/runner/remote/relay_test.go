// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner/remote"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire/runnertest"
)

// relayRunner connects a runner over loopback to a substrate whose relay sink
// is sink (nil: no relay served).
func relayRunner(t *testing.T, sink func(string, net.Conn) error) *runnertest.Server {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	link := &remote.Link{}
	remote.New("r1", link, remote.Options{Relay: sink})
	org, run := runnerwire.Loopback()
	peer := link.Attach(ctx, org)
	srv := runnertest.Serve(ctx, run, "r1", runnertest.NewFake())
	t.Cleanup(func() { peer.Close(); srv.Peer.Close() })
	return srv
}

func TestRelay_OpenWithTargetOrNoSinkRefused(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	got := make(chan net.Conn, 2)
	srv := relayRunner(t, func(id string, c net.Conn) error {
		if id != "r1" {
			t.Errorf("relay tagged with %q, want the session's runner r1", id)
		}
		got <- c
		return nil
	})
	if _, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay, Target: "10.0.0.1:443"}); err == nil {
		t.Fatal("a relay naming a target was accepted: the runner chose where the org dials")
	}
	if len(got) != 0 {
		t.Fatal("a refused relay reached the sink")
	}

	st, err := srv.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
	if err != nil {
		t.Fatalf("relay with no target: %v", err)
	}
	c := <-got
	if c.RemoteAddr().String() != "runner:r1" {
		t.Fatalf("relay conn remote = %q", c.RemoteAddr())
	}
	if _, err := st.Write([]byte("client hello")); err != nil {
		t.Fatal(err)
	}
	_ = st.CloseWrite()
	if b, err := io.ReadAll(c); err != nil || string(b) != "client hello" {
		t.Fatalf("org read %q, %v", b, err)
	}
	if _, err := c.Write([]byte("server hello")); err != nil {
		t.Fatal(err)
	}
	_ = c.(interface{ CloseWrite() error }).CloseWrite()
	if b, err := io.ReadAll(st); err != nil || string(b) != "server hello" {
		t.Fatalf("runner read %q, %v", b, err)
	}

	none := relayRunner(t, nil)
	if _, err := none.Peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay}); err == nil {
		t.Fatal("a relay was accepted where the org serves none")
	}
}
