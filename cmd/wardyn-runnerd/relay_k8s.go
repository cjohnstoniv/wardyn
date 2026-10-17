// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build k8s

package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// errRelayPortMismatch refuses a run whose control-plane URL port is not the relay Service's port
// (design K1 §3.3): the Service exposes one port, so any other would never connect.
var errRelayPortMismatch = errors.New("relay_port_mismatch")

// k8sRelay is the k8s runner's end of the internal hop (design K1 §3.3, R4-M1). It listens on the
// pod IP at the relay target port; a run's proxy reaches it through the relay Service's ClusterIP.
// It admits a connection only from the pod IP of one of this runner's live, unfenced proxy pods,
// keeps a registry of each run's connections, and closes them when the run is fenced. Each
// admitted connection is copied, unread, over one relay stream on the runner's session, as the
// Docker relay does.
type k8sRelay struct {
	ctx         context.Context
	bind        netip.AddrPort
	servicePort uint16
	clusterIP   string
	// lookup maps a source address to its run (the k8s driver's ProxyRunByIP).
	lookup func(context.Context, netip.Addr) (uuid.UUID, bool, error)

	mu        sync.Mutex
	closed    bool
	peer      *runnerwire.Peer
	ln        net.Listener
	conns     map[net.Conn]k8sRelayConn
	fenced    map[uuid.UUID]bool
	fencedIPs map[netip.Addr]uuid.UUID
	probe     chan netip.Addr // set during the boot source probe
}

type k8sRelayConn struct {
	peer *runnerwire.Peer
	run  uuid.UUID
}

func newK8sRelay(ctx context.Context, podIP netip.Addr, targetPort, servicePort uint16, clusterIP string) *k8sRelay {
	return &k8sRelay{ctx: ctx, bind: netip.AddrPortFrom(podIP, targetPort), servicePort: servicePort, clusterIP: clusterIP,
		conns: map[net.Conn]k8sRelayConn{}, fenced: map[uuid.UUID]bool{}, fencedIPs: map[netip.Addr]uuid.UUID{}}
}

// listen binds the pod IP (never an unspecified or loopback address) at the target port.
func (r *k8sRelay) listen() error {
	a := r.bind.Addr()
	if !a.IsValid() || a.IsUnspecified() || a.IsLoopback() || a.IsMulticast() {
		return fmt.Errorf("relay: refusing to listen on %s: the k8s relay binds only the pod IP", a)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return net.ErrClosed
	}
	if r.ln != nil {
		return nil
	}
	ln, err := net.Listen("tcp", r.bind.String())
	if err != nil {
		return fmt.Errorf("relay: %w", err)
	}
	r.ln = ln
	go r.serve(ln)
	return nil
}

// Ensure is the k8s driver's ControlPlaneRelay: the run's control-plane URL must use the relay
// Service's port (443 when absent), and the proxy's hosts entry maps its host to the ClusterIP.
func (r *k8sRelay) Ensure(u *url.URL) (string, error) {
	port := uint64(443)
	if p := u.Port(); p != "" {
		var err error
		if port, err = strconv.ParseUint(p, 10, 16); err != nil {
			return "", fmt.Errorf("relay: control-plane URL port %q", p)
		}
	}
	if uint16(port) != r.servicePort {
		return "", fmt.Errorf("%w: the control-plane URL uses port %d and the relay Service exposes %d", errRelayPortMismatch, port, r.servicePort)
	}
	if err := r.listen(); err != nil {
		return "", err
	}
	return r.clusterIP, nil
}

func (r *k8sRelay) serve(ln net.Listener) {
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

func remoteAddr(c net.Conn) netip.Addr {
	ap, err := netip.ParseAddrPort(c.RemoteAddr().String())
	if err != nil {
		return netip.Addr{}
	}
	return ap.Addr().Unmap()
}

// forward admits c, registers it under its run and copies it. The fence check and the
// registration happen under one mutex hold, so a connection racing a fence is either registered
// first and then closed by it, or refused.
func (r *k8sRelay) forward(c net.Conn) {
	src := remoteAddr(c)
	r.mu.Lock()
	if probe := r.probe; probe != nil {
		r.mu.Unlock()
		select {
		case probe <- src:
		default:
		}
		_ = c.Close()
		return
	}
	r.mu.Unlock()
	run, ok, err := r.lookup(r.ctx, src)
	r.mu.Lock()
	peer := r.peer
	_, ipFenced := r.fencedIPs[src]
	if err != nil || !ok || peer == nil || r.closed || ipFenced || r.fenced[run] {
		r.mu.Unlock()
		_ = c.Close()
		return
	}
	r.conns[c] = k8sRelayConn{peer: peer, run: run}
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.conns, c)
		r.mu.Unlock()
		_ = c.Close()
	}()
	relayCopy(r.ctx, peer, c)
}

// Fence marks run fenced, refuses its recorded proxy IP from now on, and closes the run's open
// connections (after unlocking, as Offline does). Leases and sessions never clear it. An IP already
// refused for another run keeps that run as its owner: a run refused on the IP (its create rolled
// back through containment) must not take the refusal over and later release it by its own witness.
func (r *k8sRelay) Fence(run uuid.UUID, proxyIP string) {
	r.mu.Lock()
	r.fenced[run] = true
	if ip, err := netip.ParseAddr(proxyIP); err == nil {
		if _, held := r.fencedIPs[ip.Unmap()]; !held {
			r.fencedIPs[ip.Unmap()] = run
		}
	}
	var mine []net.Conn
	for c, rc := range r.conns {
		if rc.run == run {
			mine = append(mine, c)
		}
	}
	r.mu.Unlock()
	for _, c := range mine {
		_ = c.Close()
	}
}

// Unfence stops refusing run's recorded proxy IP; the run itself stays fenced. An IP since fenced
// for another run keeps that run's refusal.
func (r *k8sRelay) Unfence(run uuid.UUID, proxyIP string) {
	ip, err := netip.ParseAddr(proxyIP)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fencedIPs[ip.Unmap()] == run {
		delete(r.fencedIPs, ip.Unmap())
	}
}

// IPFenced reports whether ip is a fenced run's recorded proxy IP.
func (r *k8sRelay) IPFenced(ip string) bool {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.fencedIPs[a.Unmap()]
	return ok
}

// awaitProbe arms the boot source probe and returns the wait for the source address of the next
// connection the relay accepts. No run connection is admitted while it is armed (before Online).
func (r *k8sRelay) awaitProbe() func(context.Context) (netip.Addr, error) {
	ch := make(chan netip.Addr, 1)
	r.mu.Lock()
	r.probe = ch
	r.mu.Unlock()
	return func(ctx context.Context) (netip.Addr, error) {
		defer func() {
			r.mu.Lock()
			r.probe = nil
			r.mu.Unlock()
		}()
		select {
		case a := <-ch:
			return a, nil
		case <-ctx.Done():
			return netip.Addr{}, ctx.Err()
		}
	}
}

// Online is the session hook: new relay connections ride peer.
func (r *k8sRelay) Online(peer *runnerwire.Peer) {
	r.mu.Lock()
	r.peer = peer
	r.mu.Unlock()
}

// Offline closes every connection peer carried. A newer session's connections are left alone.
func (r *k8sRelay) Offline(peer *runnerwire.Peer) {
	r.mu.Lock()
	if r.peer == peer {
		r.peer = nil
	}
	var mine []net.Conn
	for c, rc := range r.conns {
		if rc.peer == peer {
			mine = append(mine, c)
		}
	}
	r.mu.Unlock()
	for _, c := range mine {
		_ = c.Close()
	}
}

// Close stops the listener and every connection.
func (r *k8sRelay) Close() error {
	r.mu.Lock()
	r.closed = true
	var err error
	if r.ln != nil {
		err = r.ln.Close()
	}
	conns := make([]net.Conn, 0, len(r.conns))
	for c := range r.conns {
		conns = append(conns, c)
	}
	r.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
	return err
}
