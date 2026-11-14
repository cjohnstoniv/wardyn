// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

import (
	"context"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func onlineK8sRelay() *k8sRelay {
	r := newK8sRelay(context.Background(), netip.MustParseAddr("10.244.0.12"), 8443, 443, "10.96.0.40")
	r.peer = &runnerwire.Peer{}
	return r
}

// A run lookup the API server never answers ends with the connection's first-byte window, and the
// connection is closed.
func TestK8sRelayLookupEndsWithTheFirstByteWindow(t *testing.T) {
	r := onlineK8sRelay()
	r.firstByte = 50 * time.Millisecond
	ended := make(chan struct{})
	r.lookup = func(ctx context.Context, _ netip.Addr) (uuid.UUID, bool, error) {
		<-ctx.Done()
		close(ended)
		return uuid.Nil, false, ctx.Err()
	}
	a, b := net.Pipe()
	go r.forward(srcConn{a, "10.244.1.7"})
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the run lookup outlived the first-byte window")
	}
	if !closed(t, b) {
		t.Fatal("a connection whose lookup ran out was kept")
	}
}

// At most maxLocalRelays connections wait on the run lookup; one more is closed before its lookup,
// and each refused connection gives its place back.
func TestK8sRelayCapsConnectionsWaitingOnTheLookup(t *testing.T) {
	r := onlineK8sRelay()
	r.firstByte = time.Minute
	release := make(chan struct{})
	var looked atomic.Int32
	r.lookup = func(context.Context, netip.Addr) (uuid.UUID, bool, error) {
		looked.Add(1)
		<-release
		return uuid.Nil, false, nil
	}
	held := make([]net.Conn, 0, maxLocalRelays)
	for range maxLocalRelays {
		a, b := net.Pipe()
		held = append(held, b)
		go r.forward(srcConn{a, "10.244.1.7"})
	}
	for deadline := time.Now().Add(5 * time.Second); looked.Load() < maxLocalRelays; time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("%d lookups started, want %d", looked.Load(), maxLocalRelays)
		}
	}
	a, b := net.Pipe()
	go r.forward(srcConn{a, "10.244.1.8"})
	if !closed(t, b) {
		t.Fatal("a connection past the cap was kept")
	}
	if n := looked.Load(); n != maxLocalRelays {
		t.Fatalf("%d lookups, want %d: one ran past the cap", n, maxLocalRelays)
	}
	close(release)
	for _, c := range held {
		if !closed(t, c) {
			t.Fatal("a connection with no run was kept")
		}
	}
	a, b = net.Pipe()
	go r.forward(srcConn{a, "10.244.1.9"})
	if !closed(t, b) || looked.Load() != maxLocalRelays+1 {
		t.Fatalf("after the refusals %d lookups, want %d: a refused connection kept its place", looked.Load(), maxLocalRelays+1)
	}
}

// A connection admitted to its run that never sends a byte gives its place back at the window's end.
func TestK8sRelaySilentConnectionsGiveTheirPlaceBack(t *testing.T) {
	r := onlineK8sRelay()
	r.firstByte = 20 * time.Millisecond
	run := uuid.New()
	var looked atomic.Int32
	r.lookup = func(context.Context, netip.Addr) (uuid.UUID, bool, error) {
		looked.Add(1)
		return run, true, nil
	}
	for range maxLocalRelays + 1 {
		a, b := net.Pipe()
		go r.forward(srcConn{a, "10.244.1.7"})
		if !closed(t, b) {
			t.Fatal("a silent connection was kept past its window")
		}
	}
	if n := looked.Load(); n != maxLocalRelays+1 {
		t.Fatalf("%d lookups, want %d: a silent connection kept its place", n, maxLocalRelays+1)
	}
}

// One window covers the lookup and the first byte: a lookup that uses all of it leaves a silent
// connection no time of its own.
func TestK8sRelayFirstByteGetsWhatTheLookupLeft(t *testing.T) {
	r := onlineK8sRelay()
	r.firstByte = 3 * time.Second // longer than closed's wait, so a fresh window would outlast it
	run := uuid.New()
	done := make(chan struct{})
	r.lookup = func(ctx context.Context, _ netip.Addr) (uuid.UUID, bool, error) {
		<-ctx.Done()
		close(done)
		return run, true, nil
	}
	a, b := net.Pipe()
	go r.forward(srcConn{a, "10.244.1.7"})
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the run lookup outlived the window")
	}
	if !closed(t, b) {
		t.Fatal("a silent connection got a fresh first-byte window after its lookup used the whole one")
	}
}
