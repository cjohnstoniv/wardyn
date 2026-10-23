// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// maxLocalRelays bounds the local connections still waiting for their first byte. An established
// relay holds a stream, and the organisation bounds those per runner. relayFirstByteWait bounds how
// long a connection may stay silent before it opens a relay stream: the proxy's TLS client speaks
// first, so a connection that does not is not a proxy. relayCapWarnEvery spaces the warnings for
// connections closed at the cap.
const (
	maxLocalRelays     = runnerwire.MaxStreams / 4
	relayFirstByteWait = 10 * time.Second
	relayCapWarnEvery  = time.Minute
)

// localRelay is the runner's end of the internal hop (design 0.9 §4.2). A
// local run's proxy dials its org's internal hostname, which the proxy's hosts
// entry maps to the relay pair's target; this listener on the pair's bind
// copies each connection, from its first byte on, over one relay stream on the
// runner's own session. TLS ends at the org, so only ciphertext passes through, and nothing
// is forwarded anywhere but that stream. serve sets the pair (use) only once a
// probe container reached it (findRelay), before the runner goes online.
type localRelay struct {
	ctx context.Context

	mu        sync.Mutex
	pair      relayPair // unset until use
	closed    bool
	peer      *runnerwire.Peer
	listeners map[uint16]net.Listener
	conns     map[net.Conn]*runnerwire.Peer // each copy and the session carrying it
	firstByte time.Duration                 // relayFirstByteWait when zero; tests shorten it
	waiting   int                           // connections in conns yet to send their first byte
	refused   int                           // closed at the cap since capWarnAt
	capWarnAt time.Time

	// models are the laptop local model ports served at the bind (EnsureLocalModel), modelConns
	// their open connections; modelAddr is where each is forwarded, this computer's loopback when
	// zero (tests use another loopback address).
	models     map[uint16]net.Listener
	modelConns map[net.Conn]bool
	modelAddr  netip.Addr
}

// relayBind admits only Docker's host-gateway address held by the Docker
// bridge on this host: never an unspecified (0.0.0.0, ::), loopback or
// multicast address, and never an address of another interface.
func relayBind(gateway netip.Addr, bridge []netip.Addr) error {
	switch {
	case !gateway.IsValid():
		return errors.New("relay: Docker reported no host-gateway address")
	case gateway.IsUnspecified() || gateway.IsLoopback() || gateway.IsMulticast():
		return fmt.Errorf("relay: refusing to listen on %s: the relay binds only the Docker host-gateway address", gateway)
	case !slices.Contains(bridge, gateway):
		return fmt.Errorf("relay: %s is not an address of the Docker bridge on this host", gateway)
	}
	return nil
}

// use sets where the relay listens and what a run's proxy reaches it at.
func (r *localRelay) use(p relayPair) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pair = p
}

//lint:ignore U1000 used by the docker build (production_docker.go)
func (r *localRelay) inUse() relayPair {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pair
}

// answers dials every listener the relay holds, at its bind, and closes each connection before its first
// byte, which forward drops without opening a stream. A relay no run has used yet holds none.
//
//lint:ignore U1000 used by the docker build (production_docker.go)
func (r *localRelay) answers(ctx context.Context) error {
	r.mu.Lock()
	addrs := make([]string, 0, len(r.listeners))
	for port := range r.listeners {
		addrs = append(addrs, netip.AddrPortFrom(r.pair.bind, port).String())
	}
	r.mu.Unlock()
	var d net.Dialer
	for _, a := range addrs {
		c, err := d.DialContext(ctx, "tcp", a)
		if err != nil {
			return fmt.Errorf("relay: nothing answers at %s: %w", a, err)
		}
		_ = c.Close()
	}
	return nil
}

// Ensure is the Docker driver's ControlPlaneRelay: the relay listens on its
// bind at the port of u, the run's control-plane URL, and returns the target a
// run's proxy reaches it at.
func (r *localRelay) Ensure(u *url.URL) (string, error) {
	port := uint64(443)
	if p := u.Port(); p != "" {
		var err error
		if port, err = strconv.ParseUint(p, 10, 16); err != nil || port == 0 {
			return "", fmt.Errorf("relay: control-plane URL port %q", p)
		}
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.closed:
		return "", net.ErrClosed
	case !r.pair.bind.IsValid():
		return "", errors.New("relay: no address a proxy reaches has been found yet")
	case r.listeners[uint16(port)] != nil:
		return r.pair.target, nil
	case r.models[uint16(port)] != nil:
		return "", fmt.Errorf("relay: port %d serves a local model server, not the organisation", port)
	}
	ln, err := net.Listen("tcp", netip.AddrPortFrom(r.pair.bind, uint16(port)).String())
	if err != nil {
		return "", fmt.Errorf("relay: %w", err)
	}
	r.listeners[uint16(port)] = ln
	go r.serve(ln)
	return r.pair.target, nil
}

// localModelDialWait bounds the dial of the model server on this computer.
const localModelDialWait = 5 * time.Second

// EnsureLocalModel is the Docker driver's LocalModelRelay (#2957): a run's proxy reaches a laptop
// local model at the relay target on the server's port, and that port alone is forwarded to this
// computer's loopback, where the model server listens. Nothing else is reached through it: no other
// port, address or relay stream. Where the bind is that loopback address (a VM-backed engine, whose
// route to this computer lands there) the server itself answers and nothing is opened. Once open, a
// port stays open until the runner stops, as the organisation relay's do.
func (r *localRelay) EnsureLocalModel(port uint16) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	to := cmp.Or(r.modelAddr, relayLoopback)
	switch {
	case r.closed:
		return net.ErrClosed
	case !r.pair.bind.IsValid():
		return errors.New("relay: no address a proxy reaches has been found yet")
	case r.listeners[port] != nil:
		return fmt.Errorf("relay: port %d is the organisation relay's own, so no local model server can use it", port)
	case r.models[port] != nil || r.pair.bind == to:
		return nil
	}
	ln, err := net.Listen("tcp", netip.AddrPortFrom(r.pair.bind, port).String())
	if err != nil {
		return fmt.Errorf("relay: serve the local model server's port %d at %s: %w", port, r.pair.bind, err)
	}
	if r.models == nil {
		r.models, r.modelConns = map[uint16]net.Listener{}, map[net.Conn]bool{}
	}
	r.models[port] = ln
	go r.serveModel(ln, netip.AddrPortFrom(to, port).String())
	return nil
}

// serveModel copies each connection to ln to the model server at addr, with half-close both ways.
func (r *localRelay) serveModel(ln net.Listener, addr string) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			time.Sleep(50 * time.Millisecond) // a transient accept error (EMFILE) must not end the listener
			continue
		}
		go func() {
			r.mu.Lock()
			if r.closed {
				r.mu.Unlock()
				_ = c.Close()
				return
			}
			r.modelConns[c] = true
			r.mu.Unlock()
			defer func() {
				r.mu.Lock()
				delete(r.modelConns, c)
				r.mu.Unlock()
				_ = c.Close()
			}()
			s, err := net.DialTimeout("tcp", addr, localModelDialWait)
			if err != nil {
				return
			}
			defer s.Close()
			done := make(chan struct{})
			go func() { halfCopy(s, c); close(done) }()
			halfCopy(c, s)
			<-done
		}()
	}
}

// halfCopy copies src to dst, then half-closes dst's write side (or closes it, ending the other copy).
func halfCopy(dst, src net.Conn) {
	if _, err := io.Copy(dst, src); err == nil {
		if hc, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = hc.CloseWrite()
			return
		}
	}
	_ = dst.Close()
}

func (r *localRelay) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			time.Sleep(50 * time.Millisecond) // a transient accept error (EMFILE) must not end the listener
			continue
		}
		go r.forward(c)
	}
}

// forward copies c over one relay stream, with half-close both ways. Offline,
// the connection is closed at once; the proxy's own retry recovers.
func (r *localRelay) forward(c net.Conn) {
	r.mu.Lock()
	peer := r.peer
	full := r.waiting >= maxLocalRelays
	if full {
		r.warnAtCap()
	}
	if peer == nil || r.closed || full {
		r.mu.Unlock()
		_ = c.Close()
		return
	}
	r.conns[c] = peer
	r.waiting++
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, c)
		r.mu.Unlock()
		_ = c.Close()
	}()
	first, ok := readFirstByte(c, cmp.Or(r.firstByte, relayFirstByteWait))
	r.mu.Lock()
	r.waiting--
	r.mu.Unlock()
	if ok {
		relayCopy(r.ctx, peer, c, first)
	}
}

// warnAtCap counts a connection closed at maxLocalRelays and warns at most every
// relayCapWarnEvery, with the count since the last warning. r.mu is held.
func (r *localRelay) warnAtCap() {
	r.refused++
	if time.Since(r.capWarnAt) < relayCapWarnEvery {
		return
	}
	slog.Warn("relay: local connection closed at the cap on connections yet to send a first byte",
		slog.Int("limit", maxLocalRelays), slog.Int("refused", r.refused))
	r.refused, r.capWarnAt = 0, time.Now()
}

// readFirstByte reads c's first byte within wait. A relay stream opens only once c has sent one.
func readFirstByte(c net.Conn, wait time.Duration) (byte, bool) {
	var first [1]byte
	_ = c.SetReadDeadline(time.Now().Add(wait))
	if _, err := io.ReadFull(c, first[:]); err != nil {
		return 0, false
	}
	_ = c.SetReadDeadline(time.Time{})
	return first[0], true
}

// relayCopy copies c, whose first byte was already read, over one new relay stream on peer, with
// half-close both ways.
func relayCopy(ctx context.Context, peer *runnerwire.Peer, c net.Conn, first byte) {
	st, err := peer.Open(ctx, runnerwire.Open{Kind: runnerwire.KindRelay})
	if err != nil {
		return
	}
	if _, err := st.Write([]byte{first}); err != nil {
		_ = st.Close()
		return
	}
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		if _, err := io.Copy(st, c); err == nil {
			_ = st.CloseWrite()
		} else {
			_ = st.Close() // ends the other copy's read
		}
	}()
	if _, err := io.Copy(c, st); err == nil {
		if hc, ok := c.(interface{ CloseWrite() error }); ok {
			_ = hc.CloseWrite()
		}
	} else {
		_ = c.Close() // ends the other copy's read
	}
	<-sent
	select {
	case <-st.Done():
	default:
		_ = st.Close()
	}
}

// Online is the session hook: new relay connections ride peer.
func (r *localRelay) Online(peer *runnerwire.Peer) {
	r.mu.Lock()
	r.peer = peer
	r.mu.Unlock()
}

// Offline closes every connection peer carried; its streams are reset on
// reconnect anyway. A newer session's connections are left alone.
func (r *localRelay) Offline(peer *runnerwire.Peer) {
	r.mu.Lock()
	if r.peer == peer {
		r.peer = nil
	}
	var mine []net.Conn
	for c, p := range r.conns {
		if p == peer {
			mine = append(mine, c)
		}
	}
	r.mu.Unlock()
	for _, c := range mine {
		_ = c.Close()
	}
}

// Close stops every listener and connection.
func (r *localRelay) Close() error {
	r.mu.Lock()
	r.closed = true
	var errs []error
	for _, ln := range r.listeners {
		errs = append(errs, ln.Close())
	}
	for _, ln := range r.models {
		errs = append(errs, ln.Close())
	}
	conns := make([]net.Conn, 0, len(r.conns)+len(r.modelConns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	for c := range r.modelConns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return errors.Join(errs...)
}
