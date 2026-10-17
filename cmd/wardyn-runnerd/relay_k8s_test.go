// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// srcConn is a pipe end that reports a chosen remote address.
type srcConn struct {
	net.Conn
	src string
}

func (c srcConn) RemoteAddr() net.Addr { return &net.TCPAddr{IP: net.ParseIP(c.src), Port: 40000} }

func closed(t *testing.T, c net.Conn) bool {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 1))
	return err != nil && !errors.Is(err, context.DeadlineExceeded) && !isTimeout(err)
}

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func TestK8sRelayAdmissionAndFence(t *testing.T) {
	run := uuid.New()
	r := newK8sRelay(context.Background(), netip.MustParseAddr("10.244.0.12"), 8443, 443, "10.96.0.40")
	r.lookup = func(_ context.Context, ip netip.Addr) (uuid.UUID, bool, error) {
		return run, ip == netip.MustParseAddr("10.244.1.7"), nil
	}
	// Offline (no peer yet, as before Online): every connection is closed.
	a, b := net.Pipe()
	go r.forward(srcConn{a, "10.244.1.7"})
	if !closed(t, b) {
		t.Fatal("a connection was kept before Online")
	}
	// A registered connection is closed by the run's fence, and the fence refuses its IP after.
	r.mu.Lock()
	r.peer = &runnerwire.Peer{}
	held, other := net.Pipe()
	r.conns[srcConn{held, "10.244.1.7"}] = k8sRelayConn{peer: r.peer, run: run}
	r.mu.Unlock()
	r.Fence(run, "10.244.1.7")
	if !closed(t, other) {
		t.Fatal("the fence left the run's connection open")
	}
	if !r.IPFenced("10.244.1.7") || r.IPFenced("10.244.1.8") {
		t.Fatal("IP refusal")
	}
	a, b = net.Pipe()
	go r.forward(srcConn{a, "10.244.1.7"})
	if !closed(t, b) {
		t.Fatal("a fenced run's IP was admitted")
	}
	a, b = net.Pipe()
	go r.forward(srcConn{a, "10.244.9.9"})
	if !closed(t, b) {
		t.Fatal("a source that is no live proxy pod was admitted")
	}
}

// TestK8sRelayFencedIPRefusedForAnotherRun: a fenced run's recorded IP is refused even when the
// pod list now maps it to another run, until the fenced run's proxy has a witness (Unfence), and an
// Unfence by a run that no longer holds the refusal releases nothing.
func TestK8sRelayFencedIPRefusedForAnotherRun(t *testing.T) {
	fencedRun, next := uuid.New(), uuid.New()
	r := newK8sRelay(context.Background(), netip.MustParseAddr("10.244.0.12"), 8443, 443, "10.96.0.40")
	r.lookup = func(context.Context, netip.Addr) (uuid.UUID, bool, error) { return next, true, nil }
	r.mu.Lock()
	r.peer = &runnerwire.Peer{}
	r.mu.Unlock()
	r.Fence(fencedRun, "10.244.1.7")
	a, b := net.Pipe()
	go r.forward(srcConn{a, "10.244.1.7"})
	if !closed(t, b) {
		t.Fatal("a fenced IP was admitted for another run")
	}
	r.Unfence(next, "10.244.1.7")
	if !r.IPFenced("10.244.1.7") {
		t.Fatal("an Unfence by another run released the refusal")
	}
	r.Unfence(fencedRun, "10.244.1.7")
	if r.IPFenced("10.244.1.7") {
		t.Fatal("the witnessed run's IP is still refused")
	}
}

func TestK8sRelayEnsurePort(t *testing.T) {
	r := newK8sRelay(context.Background(), netip.MustParseAddr("127.0.0.1"), 8443, 443, "10.96.0.40")
	u, _ := url.Parse("https://wardynd.wardyn.svc:8443")
	if _, err := r.Ensure(u); !errors.Is(err, errRelayPortMismatch) {
		t.Fatalf("Ensure on another port: %v", err)
	}
	u, _ = url.Parse("https://wardynd.wardyn.svc")
	if _, err := r.Ensure(u); err == nil {
		t.Fatal("the relay bound a loopback address")
	}
}

func TestK8sRelayProbeObservesSource(t *testing.T) {
	r := newK8sRelay(context.Background(), netip.MustParseAddr("10.244.0.12"), 8443, 443, "10.96.0.40")
	r.lookup = func(context.Context, netip.Addr) (uuid.UUID, bool, error) {
		t.Error("probe connection looked up as a run")
		return uuid.Nil, false, nil
	}
	wait := r.awaitProbe()
	a, b := net.Pipe()
	go r.forward(srcConn{a, "10.244.3.3"})
	got, err := wait(context.Background())
	if err != nil || got != netip.MustParseAddr("10.244.3.3") {
		t.Fatalf("probe source %v %v", got, err)
	}
	if !closed(t, b) {
		t.Fatal("the probe connection was kept")
	}
}
