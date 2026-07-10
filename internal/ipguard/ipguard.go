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
)

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
