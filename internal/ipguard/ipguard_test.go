// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ipguard

import (
	"net"
	"testing"
)

// TestPrivateReservedCoversPreStdlibTable pins the membership test to the exact
// CIDR set the package listed by hand before RFC1918/ULA moved onto the stdlib
// IsPrivate predicate — one representative address per range. Any drift here is
// a security-behavior change, not a refactor.
func TestPrivateReservedCoversPreStdlibTable(t *testing.T) {
	blocked := []string{
		"10.0.0.1",          // 10.0.0.0/8 RFC1918
		"172.16.5.4",        // 172.16.0.0/12 RFC1918
		"192.168.1.1",       // 192.168.0.0/16 RFC1918
		"100.64.0.1",        // 100.64.0.0/10 CGNAT
		"0.1.2.3",           // 0.0.0.0/8 "this network"
		"192.0.0.1",         // 192.0.0.0/24 IETF protocol assignments
		"198.19.0.1",        // 198.18.0.0/15 benchmarking
		"255.255.255.255",   // limited broadcast
		"fc00::1",           // fc00::/7 unique local
		"::ffff:10.0.0.1",   // IPv4-mapped RFC1918 must not slip past
		"::ffff:100.64.0.1", // IPv4-mapped CGNAT
	}
	for _, s := range blocked {
		if ok, _ := PrivateReserved(net.ParseIP(s)); !ok {
			t.Errorf("PrivateReserved(%s) = false, want blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		if ok, why := PrivateReserved(net.ParseIP(s)); ok {
			t.Errorf("PrivateReserved(%s) = true (%s), want allowed", s, why)
		}
	}
}

// TestNAT64EmbeddedV4 pins the smuggling recheck: a NAT64 literal yields its
// embedded IPv4, anything else yields nothing.
func TestNAT64EmbeddedV4(t *testing.T) {
	for in, want := range map[string]string{
		"64:ff9b::a9fe:a9fe":   "169.254.169.254",
		"64:ff9b:1::0a00:0001": "10.0.0.1",
	} {
		got, ok := NAT64EmbeddedV4(net.ParseIP(in))
		if !ok || !got.Equal(net.ParseIP(want)) {
			t.Errorf("NAT64EmbeddedV4(%s) = %v,%v, want %s,true", in, got, ok, want)
		}
	}
	for _, s := range []string{"8.8.8.8", "2606:4700:4700::1111", "fc00::1"} {
		if got, ok := NAT64EmbeddedV4(net.ParseIP(s)); ok {
			t.Errorf("NAT64EmbeddedV4(%s) = %v,true, want false", s, got)
		}
	}
}
