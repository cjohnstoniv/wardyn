// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io"
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
	go func() { _, _ = b.Write([]byte{0x16}) }() // a TLS ClientHello's first byte
	if !closed(t, b) {
		t.Fatal("a TLS opening from a fenced IP was admitted for another run")
	}
	// The refused run's rollback fences it on the same IP (its proxy did run there). Each release is
	// its own: the IP stays refused until both runs are witnessed, in either order.
	r.Fence(next, "10.244.1.7")
	r.Unfence(next, "10.244.1.7")
	if !r.IPFenced("10.244.1.7") {
		t.Fatal("a run refused on the IP released the first run's refusal")
	}
	r.Fence(next, "10.244.1.7")
	r.Unfence(fencedRun, "10.244.1.7")
	if !r.IPFenced("10.244.1.7") {
		t.Fatal("the first run's witness released the second run's refusal")
	}
	r.Unfence(next, "10.244.1.7")
	if r.IPFenced("10.244.1.7") {
		t.Fatal("the IP is still refused after both runs were witnessed")
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

// TestK8sRelayKeyedRefusals: with run R keyed and its address fenced for run F, every opening other
// than the preamble with R's own key and the known version is closed: a TLS opening from the fenced
// address, a bad version, another key, a stalled preamble, and an unkeyed opening from an unfenced
// address of R (R holds a key, so its proxy sends only keyed connections).
func TestK8sRelayKeyedRefusals(t *testing.T) {
	run, fencedRun := uuid.New(), uuid.New()
	rKey := bytes.Repeat([]byte{1}, relayPreambleKeyLen)
	r := newK8sRelay(context.Background(), netip.MustParseAddr("10.244.0.12"), 8443, 443, "10.96.0.40")
	r.firstByte = 200 * time.Millisecond
	r.lookup = func(context.Context, netip.Addr) (uuid.UUID, bool, error) { return run, true, nil }
	r.mu.Lock()
	r.peer = &runnerwire.Peer{}
	r.mu.Unlock()
	r.Fence(fencedRun, "10.244.1.7")
	r.RelayKey(run, sha256.Sum256(rKey))
	for _, c := range []struct {
		name, src string
		send      []byte
	}{
		{"TLS from the fenced address", "10.244.1.7", []byte{0x16}},
		{"bad version", "10.244.1.7", append(preamble(relayPreambleVersion+1, rKey), 0x16)},
		{"another key", "10.244.1.7", append(preamble(relayPreambleVersion, bytes.Repeat([]byte{2}, relayPreambleKeyLen)), 0x16)},
		{"stalled preamble", "10.244.1.7", preamble(relayPreambleVersion, rKey[:10])},
		{"unkeyed from an unfenced address of a keyed run", "10.244.1.8", []byte{0x16}},
	} {
		a, b := net.Pipe()
		go r.forward(srcConn{a, c.src})
		go func() { _, _ = b.Write(c.send) }()
		if !closed(t, b) {
			t.Fatalf("%s: admitted", c.name)
		}
	}
}

// TestK8sRelayAdmitsKeyedConnectionOnFencedAddress: with a usable peer, a connection that opens with
// the preamble carrying run R's own key is admitted from an address fenced for another run F, and
// the stream after the preamble reaches the org untouched, first byte first; F's own key is not.
func TestK8sRelayAdmitsKeyedConnectionOnFencedAddress(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opened := make(chan *runnerwire.Stream, 1)
	orgConn, runConn := runnerwire.Loopback()
	org := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnOpen: func(s *runnerwire.Stream, o runnerwire.Open) error {
		if o.Kind != runnerwire.KindRelay {
			t.Errorf("runner opened %+v", o)
		}
		opened <- s
		return nil
	}})
	peer := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	go func() { _ = org.Run(ctx) }()
	go func() { _ = peer.Run(ctx) }()

	run, fencedRun := uuid.New(), uuid.New()
	rKey := bytes.Repeat([]byte{1}, relayPreambleKeyLen)
	fKey := bytes.Repeat([]byte{2}, relayPreambleKeyLen)
	r := newK8sRelay(ctx, netip.MustParseAddr("10.244.0.12"), 8443, 443, "10.96.0.40")
	r.lookup = func(context.Context, netip.Addr) (uuid.UUID, bool, error) { return run, true, nil }
	r.Online(peer)
	r.Fence(fencedRun, "10.244.1.7")
	r.RelayKey(run, sha256.Sum256(rKey))
	r.RelayKey(fencedRun, sha256.Sum256(fKey)) // refused: a fenced run gets no key

	a, b := net.Pipe()
	go r.forward(srcConn{a, "10.244.1.7"})
	go func() { _, _ = b.Write(append(preamble(relayPreambleVersion, rKey), 0x16, 'h', 'i')) }()
	var st *runnerwire.Stream
	select {
	case st = <-opened:
	case <-ctx.Done():
		t.Fatal("the run's keyed connection from the fenced address was not admitted")
	}
	buf := make([]byte, 3)
	if _, err := io.ReadFull(st, buf); err != nil || !bytes.Equal(buf, []byte{0x16, 'h', 'i'}) {
		t.Fatalf("org read %q, %v, want the stream after the preamble", buf, err)
	}
	r.mu.Lock()
	registered := len(r.conns)
	r.mu.Unlock()
	if registered != 1 {
		t.Fatalf("%d connections registered, want 1", registered)
	}

	a, b = net.Pipe()
	go r.forward(srcConn{a, "10.244.1.7"})
	go func() { _, _ = b.Write(append(preamble(relayPreambleVersion, fKey), 0x16)) }()
	if !closed(t, b) {
		t.Fatal("the fenced run's key was admitted")
	}
}

// TestK8sRelayKeyState: RelayKey holds a run's key hash, Fence drops it and a fenced run gets none
// again, DropRelayKey forgets it, and FencedRuns lists the runs whose fence refuses an IP.
func TestK8sRelayKeyState(t *testing.T) {
	run, other, fencedRun := uuid.New(), uuid.New(), uuid.New()
	h := sha256.Sum256([]byte("k"))
	r := newK8sRelay(context.Background(), netip.MustParseAddr("10.244.0.12"), 8443, 443, "10.96.0.40")
	held := func(id uuid.UUID) bool {
		r.mu.Lock()
		defer r.mu.Unlock()
		_, ok := r.keys[id]
		return ok
	}
	r.RelayKey(run, h)
	r.RelayKey(other, h)
	if !held(run) || !held(other) {
		t.Fatal("RelayKey held no key")
	}
	r.Fence(fencedRun, "10.244.1.7")
	r.Fence(run, "10.244.1.7")
	r.RelayKey(run, h)
	if held(run) {
		t.Fatal("a fenced run holds a key")
	}
	r.DropRelayKey(other)
	if held(other) {
		t.Fatal("DropRelayKey kept the key")
	}
	if got := r.FencedRuns("10.244.1.7"); len(got) != 2 {
		t.Fatalf("FencedRuns %v, want both runs", got)
	}
	r.Unfence(fencedRun, "10.244.1.7")
	if got := r.FencedRuns("::ffff:10.244.1.7"); len(got) != 1 || got[0] != run {
		t.Fatalf("FencedRuns after one witness %v", got)
	}
	if got := r.FencedRuns("not an ip"); got != nil {
		t.Fatalf("FencedRuns of a bad IP %v", got)
	}
}
