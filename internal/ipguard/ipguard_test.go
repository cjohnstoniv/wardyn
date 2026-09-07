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

// F089: the gateway predicate's whole table, in the ONE place it now lives.
//
// It used to be two byte-identical unexported copies (api.llmGatewayIPRefused,
// proxy.trustedGatewayIPRefused) coupled by a comment, and the proxy copy's
// NAT64 arm was unpinned: replacing it with `return false` left the entire
// internal/egress/proxy suite green, while a gateway hostname resolving to
// 64:ff9b::a9fe:a9fe — NAT64-mapped 169.254.169.254 — would be admitted by the
// per-request re-check and dialled with the brokered model credential.
//
// Both halves matter and both are listed: the refusals are the trust boundary,
// and the admissions are why this is not PrivateReserved — an internal gateway
// is EXPECTED to live on RFC1918/ULA/CGNAT, so refusing those would break the
// feature rather than secure it.
func TestGatewayIPRefused(t *testing.T) {
	for _, ip := range []string{
		"127.0.0.1", "::1", "::ffff:127.0.0.1", // loopback, incl. v4-mapped
		"169.254.169.254", "fe80::1", // link-local, the metadata address included
		"224.0.0.1", "ff02::1", // multicast
		"0.0.0.0", "::", // unspecified
		"64:ff9b::a9fe:a9fe", "64:ff9b::808:808", "64:ff9b:1::a9fe:a9fe", // NAT64-embedded
	} {
		if !GatewayIPRefused(net.ParseIP(ip)) {
			t.Errorf("GatewayIPRefused(%s) = false, want true — a model gateway can never legitimately be this address", ip)
		}
	}
	for _, ip := range []string{
		"10.0.0.5", "172.16.0.1", "192.168.1.1", "fd00::1", "100.64.0.1", // where an internal gateway lives
		"0.1.2.3", "192.0.0.1", "198.18.0.1", "255.255.255.255", // reserved, but not this predicate's business
		"8.8.8.8", "2001:4860:4860::8888",
	} {
		if GatewayIPRefused(net.ParseIP(ip)) {
			t.Errorf("GatewayIPRefused(%s) = true, want false — refusing it would break the internal-gateway feature", ip)
		}
	}
	// An address that is not an address at all fails CLOSED.
	if !GatewayIPRefused(nil) {
		t.Error("GatewayIPRefused(nil) = false, want true (fail closed)")
	}
}
