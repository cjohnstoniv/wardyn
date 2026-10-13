// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/netip"
	"net/url"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

func TestRunnerRelayListenerBindsOnlyTheGateway(t *testing.T) {
	gw := netip.MustParseAddr("172.17.0.1")
	bridge := []netip.Addr{gw}
	for _, tc := range []struct {
		name   string
		addr   netip.Addr
		bridge []netip.Addr
		ok     bool
	}{
		{"the bridge's host-gateway", gw, bridge, true},
		{"every IPv4 interface", netip.MustParseAddr("0.0.0.0"), append(bridge, netip.MustParseAddr("0.0.0.0")), false},
		{"every IPv6 interface", netip.MustParseAddr("::"), append(bridge, netip.MustParseAddr("::")), false},
		{"loopback", netip.MustParseAddr("127.0.0.1"), append(bridge, netip.MustParseAddr("127.0.0.1")), false},
		{"another interface's address", netip.MustParseAddr("192.168.1.20"), bridge, false},
		{"no gateway reported", netip.Addr{}, bridge, false},
	} {
		if err := relayBind(tc.addr, tc.bridge); (err == nil) != tc.ok {
			t.Errorf("%s: relayBind(%s) = %v, want ok=%v", tc.name, tc.addr, err, tc.ok)
		}
	}
	u, _ := url.Parse("https://wardynd.internal:8443")
	r := newLocalRelay(context.Background(), netip.MustParseAddr("0.0.0.0"), []netip.Addr{netip.MustParseAddr("0.0.0.0")}, nil)
	if err := r.Ensure(u); err == nil || len(r.listeners) != 0 {
		t.Fatalf("a relay bound the unspecified address: %v", err)
	}
}

// TestRunnerRelayForwardsOverItsOwnStream drives localRelay's copy loop on a
// loopback listener (relayBind is bypassed: no Docker bridge here): bytes go
// both ways over one relay stream with no target, and the connection closes
// when its session goes offline.
func TestRunnerRelayForwardsOverItsOwnStream(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	opened := make(chan *runnerwire.Stream, 1)
	orgConn, runConn := runnerwire.Loopback()
	org := runnerwire.NewPeer(orgConn, runnerwire.PeerConfig{Org: true, OnOpen: func(s *runnerwire.Stream, o runnerwire.Open) error {
		if o.Kind != runnerwire.KindRelay || o.Target != "" {
			t.Errorf("runner opened %+v", o)
		}
		opened <- s
		return nil
	}})
	run := runnerwire.NewPeer(runConn, runnerwire.PeerConfig{})
	go func() { _ = org.Run(ctx) }()
	go func() { _ = run.Run(ctx) }()

	r := &localRelay{ctx: ctx, gateway: netip.MustParseAddr("127.0.0.1"), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}}
	defer r.Close()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	r.listeners[1] = ln
	go r.serve(ln)

	offline, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := offline.Read(make([]byte, 1)); n != 0 {
		t.Fatal("an offline relay carried bytes")
	}

	r.Online(run)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Write([]byte("hello org")); err != nil {
		t.Fatal(err)
	}
	st := <-opened
	buf := make([]byte, 9)
	if _, err := io.ReadFull(st, buf); err != nil || string(buf) != "hello org" {
		t.Fatalf("org read %q, %v", buf, err)
	}
	if _, err := st.Write([]byte("hello proxy")); err != nil {
		t.Fatal(err)
	}
	buf = make([]byte, 11)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello proxy" {
		t.Fatalf("proxy read %q, %v", buf, err)
	}
	r.Offline(run)
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	if _, err := c.Read(make([]byte, 1)); err == nil {
		t.Fatal("the relayed connection outlived its session")
	}
}
