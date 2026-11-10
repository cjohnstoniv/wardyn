// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"io"
	"net"
	"net/netip"
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

			rows := 0
			for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if rows = len(dnsRows(t, buf)); rows > 0 && tc.allowed {
					break
				}
			}
			if tc.allowed && rows == 0 {
				t.Error("no DNS row for an allowed peer")
			}
			if !tc.allowed && rows != 0 {
				t.Errorf("%d DNS rows for a caller outside the peers, want 0", rows)
			}
		})
	}
}
