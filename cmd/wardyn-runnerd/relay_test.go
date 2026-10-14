// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"io"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strings"
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
}

var (
	testGateway = netip.MustParseAddr("172.17.0.1")
	testNative  = dockerEngine{desc: `"Ubuntu 24.04 LTS" (laptop)`, native: true, gateway: testGateway, bridge: []netip.Addr{testGateway}}
	testVM      = dockerEngine{desc: `"Alpine Linux v3.21" (lima-rancher-desktop)`, why: "its bridge interface is not on this host"}
	testVeth    = netip.MustParseAddr("192.168.143.2")
)

func pair(bind, target string) relayPair { return relayPair{netip.MustParseAddr(bind), target} }

// The search list: native has one pair, a VM-backed engine the known routes in order, the veth pair
// only where this host holds veth-rd-wsl; explicit values narrow it and are never replaced.
func TestRelayCandidates(t *testing.T) {
	vmVeth := testVM
	vmVeth.veth = []netip.Addr{testVeth}
	vm := []relayPair{pair("127.0.0.1", "host-gateway"), pair("127.0.0.1", "192.168.5.2"), pair("127.0.0.1", "192.168.127.254")}
	for _, tc := range []struct {
		name string
		eng  dockerEngine
		want relayPair
		out  []relayPair
	}{
		{"native", testNative, relayPair{}, []relayPair{pair("172.17.0.1", "host-gateway")}},
		{"VM-backed", testVM, relayPair{}, vm},
		{"VM-backed with veth-rd-wsl", vmVeth, relayPair{}, []relayPair{vm[0], pair("192.168.143.2", "192.168.143.2"), vm[1], vm[2]}},
		{"an explicit bind filters", vmVeth, relayPair{bind: relayLoopback}, vm},
		{"an explicit target filters", testVM, relayPair{target: "192.168.5.2"}, vm[1:2]},
		{"an unmatched explicit target is kept", testVM, relayPair{target: "192.0.2.1"}, []relayPair{pair("127.0.0.1", "192.0.2.1")}},
		{"an unmatched explicit pair is exactly that pair", testVM, pair("192.168.143.2", "192.0.2.1"), []relayPair{pair("192.168.143.2", "192.0.2.1")}},
		{"native with loopback", testNative, relayPair{bind: relayLoopback}, []relayPair{pair("127.0.0.1", "host-gateway")}},
	} {
		if got := relayCandidates(tc.eng, tc.want); !slices.Equal(got, tc.out) {
			t.Errorf("%s: relayCandidates = %v, want %v", tc.name, got, tc.out)
		}
	}
}

// The relay's bind is a security rule, the same on every engine: loopback, a veth-rd-wsl address or a
// native bridge's gateway, never an address another computer reaches.
func TestAdmitRelayBind(t *testing.T) {
	vmVeth := testVM
	vmVeth.veth = []netip.Addr{testVeth}
	noBridge := testNative
	noBridge.bridge = nil
	for _, tc := range []struct {
		name string
		addr string
		eng  dockerEngine
		ok   bool
	}{
		{"every IPv4 interface, native", "0.0.0.0", testNative, false},
		{"every IPv4 interface, VM-backed", "0.0.0.0", testVM, false},
		{"every IPv6 interface, native", "::", testNative, false},
		{"every IPv6 interface, VM-backed", "::", testVM, false},
		{"multicast, native", "224.0.0.1", testNative, false},
		{"multicast, VM-backed", "ff02::1", testVM, false},
		{"loopback, native", "127.0.0.1", testNative, true},
		{"loopback, VM-backed", "127.0.0.1", testVM, true},
		{"IPv6 loopback, VM-backed", "::1", testVM, true},
		{"the native gateway its bridge holds", "172.17.0.1", testNative, true},
		{"the native gateway its bridge does not hold", "172.17.0.1", noBridge, false},
		{"a LAN address, native", "192.168.1.20", testNative, false},
		{"a LAN address, VM-backed", "192.168.1.20", testVM, false},
		{"a held veth-rd-wsl address", "192.168.143.2", vmVeth, true},
		{"a veth-rd-wsl address this host does not hold", "192.168.143.2", testVM, false},
		{"the gateway on a VM-backed engine", "172.17.0.1", dockerEngine{gateway: testGateway, bridge: []netip.Addr{testGateway}}, false},
	} {
		if err := admitRelayBind(netip.MustParseAddr(tc.addr), tc.eng); (err == nil) != tc.ok {
			t.Errorf("%s: admitRelayBind(%s) = %v, want ok=%v", tc.name, tc.addr, err, tc.ok)
		}
	}
}

// A listener a container reaches only after a moment (Docker Desktop on WSL2) is retried on the same
// pair, not skipped for the next.
func TestFindRelayRetriesAPairBeforeMovingOn(t *testing.T) {
	quickAttempts(t)
	calls := 0
	reachesOnTheThird := func(ctx context.Context, target string, port uint16) error {
		calls++
		if calls < 3 {
			return errors.New("did not connect")
		}
		return dialProbe(target)(ctx, target, port)
	}
	if got, err := findRelay(context.Background(), reachesOnTheThird, testVM, relayPair{}); err != nil || got != pair("127.0.0.1", "host-gateway") || calls != 3 {
		t.Fatalf("findRelay = %v, %v after %d probes; want the first pair on the third", got, err, calls)
	}
}

// Until serve has found where a proxy reaches it, the relay listens nowhere; then it listens on the
// pair's bind and hands the driver the pair's target.
func TestRelayEnsureNeedsAPair(t *testing.T) {
	r := &localRelay{ctx: context.Background(), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}}
	defer r.Close()
	u := &url.URL{Scheme: "https", Host: "wardynd.internal:1"}
	if _, err := r.Ensure(u); err == nil {
		t.Fatal("Ensure listened before any pair was set")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	u.Host = ln.Addr().String() // a port free a moment ago
	_ = ln.Close()
	r.use(pair("127.0.0.1", "192.168.5.2"))
	if target, err := r.Ensure(u); err != nil || target != "192.168.5.2" {
		t.Fatalf("Ensure = %q, %v; want the pair's target", target, err)
	}
}

// dialProbe is a probe container that reaches this host's loopback only through the targets listed.
func dialProbe(reach ...string) relayProbe {
	return func(_ context.Context, target string, port uint16) error {
		if !slices.Contains(reach, target) {
			return errors.New("did not connect")
		}
		c, err := net.Dial("tcp", netip.AddrPortFrom(relayLoopback, port).String())
		if err != nil {
			return err
		}
		return c.Close()
	}
}

// quickAttempts runs relayAttempts' four probes back to back.
func quickAttempts(t *testing.T) {
	old := relayAttempts
	relayAttempts = []time.Duration{0, 0, 0, 0}
	t.Cleanup(func() { relayAttempts = old })
}

// A pair passes only when the probe passes and the runner's own listener accepted its connection.
func TestFindRelayRequiresTheListenerToAccept(t *testing.T) {
	quickAttempts(t)
	ctx := context.Background()
	answeredElsewhere := func(context.Context, string, uint16) error { return nil } // exit 0, nothing dialled here
	if _, err := findRelay(ctx, answeredElsewhere, testVM, relayPair{target: "192.168.5.2"}); err == nil || !strings.Contains(err.Error(), "not to this runner's listener") {
		t.Fatalf("a probe that never reached the listener passed: %v", err)
	}
	if got, err := findRelay(ctx, dialProbe("host-gateway"), testVM, relayPair{}); err != nil || got != pair("127.0.0.1", "host-gateway") {
		t.Fatalf("findRelay = %v, %v; want the first pair", got, err)
	}
	if got, err := findRelay(ctx, dialProbe("192.168.5.2"), testVM, relayPair{}); err != nil || got != pair("127.0.0.1", "192.168.5.2") {
		t.Fatalf("findRelay = %v, %v; want the first pair that passes", got, err)
	}
	_, err := findRelay(ctx, dialProbe(), testVM, relayPair{})
	for _, want := range []string{`"Alpine Linux v3.21" (lima-rancher-desktop)`, "listen 127.0.0.1, reach at host-gateway: did not connect",
		"reach at 192.168.5.2", "reach at 192.168.127.254", "4 attempts", "--relay-listen", "WARDYN_RELAY_LISTEN", "--relay-host-target", "WARDYN_RELAY_HOST_TARGET"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("no pair passed, and the refusal lacks %q: %v", want, err)
		}
	}
	if _, err := findRelay(ctx, dialProbe("host-gateway"), testNative, relayPair{bind: netip.MustParseAddr("0.0.0.0")}); err == nil || !strings.Contains(err.Error(), "refusing to listen on 0.0.0.0") {
		t.Fatalf("an unspecified bind was probed: %v", err)
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

	r := &localRelay{ctx: ctx, pair: pair("127.0.0.1", "host-gateway"), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}}
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
