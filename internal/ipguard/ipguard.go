// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package ipguard holds the SSRF private/reserved membership test shared by the
// egress proxy's policy guard (internal/egress/proxy) and the composer transport
// guard (internal/composer/backends/transport). MEMBERSHIP ONLY, by design: each
// consumer keeps its own predicate around it because their semantics
// deliberately differ — the proxy unconditionally denies loopback/link-local,
// while the transport must keep loopback denied under its operator-only
// allowPrivate escape hatch. Both reach for net.IP's own predicates first; this
// package carries only what the stdlib does not (netip.Addr.IsPrivate covers
// RFC1918 and the IPv6 ULA range, so those are NOT re-listed here).
package ipguard

import (
	"net"
	"net/netip"
	"slices"
)

// ReservedV4 are the reserved IPv4 ranges every guard denies that
// netip.Addr.IsPrivate does not already cover.
var ReservedV4 = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT (RFC6598)
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("255.255.255.255/32"),
}

// NAT64Prefixes are the well-known + local-use NAT64 translation prefixes
// (RFC 6052 / RFC 8215). An address inside one carries a real IPv4 in its
// low 32 bits, so a private/metadata target can be smuggled as an IPv6
// literal (64:ff9b::a9fe:a9fe -> 169.254.169.254) past every stdlib
// predicate (To4() is nil for it). Every guard must block these wholesale
// and re-check the embedded v4 so the denial names the real target.
var NAT64Prefixes = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"),   // well-known NAT64 (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64 (RFC 8215)
}

// PrivateReserved reports whether ip is in a private/reserved range every guard
// denies regardless of mode — RFC1918 and the IPv6 ULA range fc00::/7 (stdlib
// IsPrivate) plus ReservedV4 — and names the matching range for the denial
// reason. IPv4-mapped IPv6 addresses are unwrapped first.
func PrivateReserved(ip net.IP) (bool, string) {
	addr, ok := addrOf(ip)
	if !ok {
		return false, ""
	}
	if addr.IsPrivate() {
		return true, "rfc1918/ula"
	}
	if i := slices.IndexFunc(ReservedV4, func(p netip.Prefix) bool { return p.Contains(addr) }); i >= 0 {
		return true, ReservedV4[i].String()
	}
	return false, ""
}

// NAT64EmbeddedV4 returns the IPv4 embedded in the low 32 bits of ip and true
// when ip falls inside a NAT64 prefix; otherwise (nil, false). Callers block
// the prefix wholesale and re-run the embedded v4 through their own v4 guard so
// a NAT64-smuggled private/metadata target cannot slip past To4()==nil.
func NAT64EmbeddedV4(ip net.IP) (net.IP, bool) {
	addr, ok := addrOf(ip)
	if !ok || !slices.ContainsFunc(NAT64Prefixes, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return nil, false
	}
	b := addr.As16()
	return net.IP(b[12:16]), true
}

// addrOf converts a net.IP to a comparable netip.Addr, unmapping IPv4-mapped
// IPv6 so "::ffff:10.0.0.1" is tested as the IPv4 address it is.
func addrOf(ip net.IP) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	return addr.Unmap(), ok
}
