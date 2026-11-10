// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net"
	"net/netip"
	"slices"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// freeDNSAddr is a loopback address whose port is free for both UDP and TCP.
func freeDNSAddr(t *testing.T) string {
	t.Helper()
	for range 50 {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		_ = ln.Close()
		pc, err := net.ListenPacket("udp", addr)
		if err != nil {
			continue
		}
		_ = pc.Close()
		return addr
	}
	t.Fatal("no loopback port free for both UDP and TCP")
	return ""
}

// TestListenDNSServesOnlyAllowedPeers: with AllowedPeers set, the listener
// serves a caller inside them over UDP and TCP and writes its row, and gives a
// caller outside them no reply on UDP, a closed connection on TCP, and no row.
func TestListenDNSServesOnlyAllowedPeers(t *testing.T) {
	for _, tc := range []struct {
		name    string
		peers   []netip.Prefix
		allowed bool
	}{
		{"caller inside the peers", []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8")}, true},
		{"caller outside the peers", []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, false},
		{"no peers set", nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, buf := newRuleTestProxy(t, types.RunPolicySpec{}, "127.0.0.1:1", true, "", nil)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			addr := freeDNSAddr(t)
			if err := (&Server{proxy: p, allowedPeers: tc.peers}).ListenDNS(ctx, addr); err != nil {
				t.Fatal(err)
			}

			c, err := net.Dial("udp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(time.Second))
			if _, err := c.Write(dnsQueryWire(t, "peer.example.com.", dnsmessage.TypeA)); err != nil {
				t.Fatal(err)
			}
			rb := make([]byte, 1500)
			n, uerr := c.Read(rb)
			if tc.allowed {
				if uerr != nil {
					t.Fatalf("UDP query from an allowed peer: %v", uerr)
				}
				checkRefused(t, rb[:n], "peer.example.com.")
			} else if uerr == nil {
				t.Fatalf("UDP query from outside the peers got a %d byte reply", n)
			}

			tcp, err := net.Dial("tcp", addr)
			if err != nil {
				t.Fatal(err)
			}
			defer tcp.Close()
			_ = tcp.SetDeadline(time.Now().Add(time.Second))
			q := dnsQueryWire(t, "peer.example.com.", dnsmessage.TypeTXT)
			_, _ = tcp.Write(append([]byte{byte(len(q) >> 8), byte(len(q))}, q...))
			var size [2]byte
			_, terr := io.ReadFull(tcp, size[:])
			if tc.allowed != (terr == nil) {
				t.Fatalf("TCP query, allowed=%v: read error %v", tc.allowed, terr)
			}

			// One row per protocol: the UDP query asked for A, the TCP query for TXT.
			var kinds []string
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				kinds = kinds[:0]
				for _, r := range dnsRows(t, buf) {
					kinds = append(kinds, r.Type)
				}
				slices.Sort(kinds)
				if tc.allowed && slices.Equal(kinds, []string{"A", "TXT"}) {
					break
				}
			}
			if tc.allowed && !slices.Equal(kinds, []string{"A", "TXT"}) {
				t.Errorf("DNS row types for an allowed peer = %v, want [A TXT] (the UDP row and the TCP row)", kinds)
			}
			if !tc.allowed && len(kinds) != 0 {
				t.Errorf("DNS rows %v for a caller outside the peers, want none", kinds)
			}
		})
	}
}

// textAddr is an address that prints exactly as given, so an IPv4-mapped spelling reaches the filter as written
// (net's own address types print it unmapped).
type textAddr string

func (a textAddr) Network() string { return "udp" }
func (a textAddr) String() string  { return string(a) }

// stubPacketConn hands out one datagram from a chosen source, then fails.
type stubPacketConn struct {
	net.PacketConn
	from []net.Addr
}

func (c *stubPacketConn) ReadFrom(b []byte) (int, net.Addr, error) {
	if len(c.from) == 0 {
		return 0, nil, net.ErrClosed
	}
	a := c.from[0]
	c.from = c.from[1:]
	return copy(b, "x"), a, nil
}

// TestPeerPacketConnFiltersLikeThePeerListener: a datagram from inside AllowedPeers is delivered, an IPv4-mapped
// form of an allowed address is too (as peerListener treats it), and one from a foreign address on the run's
// shared network is dropped before the reader sees it.
func TestPeerPacketConnFiltersLikeThePeerListener(t *testing.T) {
	allowed := []netip.Prefix{netip.MustParsePrefix("172.20.0.0/16")}
	addr := func(s string) net.Addr { return textAddr(s) }
	c := &peerPacketConn{
		PacketConn: &stubPacketConn{from: []net.Addr{
			addr("10.9.9.9:5000"),            // foreign wardyn-internal source: dropped
			addr("[::ffff:10.9.9.9]:5000"),   // the same, IPv4-mapped: dropped
			addr("172.20.0.2:5000"),          // allowed
			addr("[::ffff:172.20.0.3]:5000"), // allowed, IPv4-mapped
		}},
		allowed: allowed,
	}
	var got []string
	buf := make([]byte, 16)
	for {
		_, from, err := c.ReadFrom(buf)
		if err != nil {
			break
		}
		got = append(got, from.String())
	}
	if len(got) != 2 || got[0] != "172.20.0.2:5000" || got[1] != "[::ffff:172.20.0.3]:5000" {
		t.Fatalf("delivered %v, want only the two sources inside the peers", got)
	}
}

type stubConn struct {
	net.Conn
	remote net.Addr
	closed bool
}

func (c *stubConn) RemoteAddr() net.Addr { return c.remote }
func (c *stubConn) Close() error         { c.closed = true; return nil }

type stubListener struct {
	net.Listener
	conns []*stubConn
}

func (l *stubListener) Accept() (net.Conn, error) {
	if len(l.conns) == 0 {
		return nil, net.ErrClosed
	}
	c := l.conns[0]
	l.conns = l.conns[1:]
	return c, nil
}

// TestPeerListenerFiltersDNSConnections: the same sources over TCP: a foreign one is closed before a byte is read,
// an allowed one and its IPv4-mapped form are accepted.
func TestPeerListenerFiltersDNSConnections(t *testing.T) {
	tcp := func(s string) *stubConn {
		return &stubConn{remote: net.TCPAddrFromAddrPort(netip.MustParseAddrPort(s))}
	}
	foreign, mappedForeign, ok, mapped := tcp("10.9.9.9:5000"), tcp("[::ffff:10.9.9.9]:5000"), tcp("172.20.0.2:5000"), tcp("[::ffff:172.20.0.3]:5000")
	l := &peerListener{
		Listener: &stubListener{conns: []*stubConn{foreign, mappedForeign, ok, mapped}},
		allowed:  []netip.Prefix{netip.MustParsePrefix("172.20.0.0/16")},
	}
	var got []net.Conn
	for {
		c, err := l.Accept()
		if err != nil {
			break
		}
		got = append(got, c)
	}
	if len(got) != 2 || got[0] != net.Conn(ok) || got[1] != net.Conn(mapped) || !foreign.closed || !mappedForeign.closed || ok.closed || mapped.closed {
		t.Fatalf("accepted %d; foreign closed %v/%v, allowed closed %v/%v; want only the two inside the peers", len(got), foreign.closed, mappedForeign.closed, ok.closed, mapped.closed)
	}
}
