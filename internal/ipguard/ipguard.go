// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package ipguard holds the SSRF-blocklist CIDR table shared by the egress
// proxy's policy guard (internal/egress/proxy) and the composer transport
// guard (internal/composer/backends/transport). TABLE ONLY, by design: each
// consumer keeps its own predicate because their semantics deliberately
// differ — the proxy unconditionally denies loopback/link-local (carried
// in-table there) and re-checks NAT64-embedded IPv4, while the transport
// handles loopback/link-local via net.IP predicates so they stay blocked
// even under its operator-only allowPrivate escape hatch.
package ipguard

import "net"

var (
	// PrivateReservedV4 are the private/reserved IPv4 ranges every guard
	// denies regardless of mode: RFC1918, CGNAT (RFC6598), "this network",
	// IETF protocol assignments, benchmarking, and limited broadcast.
	PrivateReservedV4 = MustCIDRs(
		"10.0.0.0/8",     // RFC1918 private
		"172.16.0.0/12",  // RFC1918 private
		"192.168.0.0/16", // RFC1918 private
		"100.64.0.0/10",  // CGNAT (RFC6598)
		"0.0.0.0/8",      // "this network"
		"192.0.0.0/24",   // IETF protocol assignments
		"198.18.0.0/15",  // benchmarking
		"255.255.255.255/32",
	)

	// UniqueLocalV6 is the IPv6 unique-local (ULA) range every guard denies.
	UniqueLocalV6 = MustCIDRs(
		"fc00::/7", // unique local (ULA)
	)

	// NAT64Prefixes are the well-known + local-use NAT64 translation prefixes
	// (RFC 6052 / RFC 8215). An address inside one carries a real IPv4 in its
	// low 32 bits, so a private/metadata target can be smuggled as an IPv6
	// literal (64:ff9b::a9fe:a9fe -> 169.254.169.254) past every stdlib
	// predicate (To4() is nil for it). Every guard must block these wholesale
	// and re-check the embedded v4 so the denial names the real target.
	NAT64Prefixes = MustCIDRs(
		"64:ff9b::/96",   // well-known NAT64 (RFC 6052)
		"64:ff9b:1::/48", // local-use NAT64 (RFC 8215)
	)
)

// NAT64EmbeddedV4 returns the IPv4 embedded in the low 32 bits of ip and true
// when ip falls inside a NAT64 prefix; otherwise (nil, false). Callers block
// the prefix wholesale and re-run the embedded v4 through their own v4 guard so
// a NAT64-smuggled private/metadata target cannot slip past To4()==nil.
func NAT64EmbeddedV4(ip net.IP) (net.IP, bool) {
	ip16 := ip.To16()
	if ip16 == nil {
		return nil, false
	}
	for _, n := range NAT64Prefixes {
		if n.Contains(ip) {
			return net.IP(ip16[12:16]), true
		}
	}
	return nil, false
}

// MustCIDRs parses CIDR literals, panicking on a bad entry — for package-level
// tables of programmer-authored constants only (init-time failure, never on a
// request path).
func MustCIDRs(cidrs ...string) []*net.IPNet {
	out := make([]*net.IPNet, 0, len(cidrs))
	for _, c := range cidrs {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			panic("ipguard: bad builtin CIDR " + c)
		}
		out = append(out, n)
	}
	return out
}
