// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package ipguard holds the SSRF private/reserved membership test shared by the egress proxy's policy
// guard and the composer transport guard. MEMBERSHIP ONLY by design: each consumer keeps its own predicate
// around it since their semantics deliberately differ. Carries only what stdlib lacks (netip.Addr.IsPrivate
// covers RFC1918 + IPv6 ULA, so those are NOT re-listed in ReservedV4 — they ARE in Liftable, a different job).
package ipguard

import (
	"net"
	"net/netip"
	"slices"
)

// ReservedV4 are the reserved IPv4 ranges every guard denies that netip.Addr.IsPrivate doesn't cover.
//
// SECURITY invariant: canonical surface is the IANA IPv4 Special-Purpose Address Registry, not a
// remembered RFC list — an entry belongs here when the registry's "Globally Reachable" column is False
// and no stdlib predicate covers it; diff against the registry, not intuition. Rows that ARE globally
// reachable (192.31.196.0/24, 192.52.193.0/24, 192.175.48.0/24) stay out.
//
// Order matters only for the reason string: PrivateReserved names the FIRST matching prefix, so
// 255.255.255.255/32 stays listed ahead of the 240.0.0.0/4 that now contains it.
var ReservedV4 = []netip.Prefix{
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT (RFC6598)
	netip.MustParsePrefix("0.0.0.0/8"),     // "this network"
	netip.MustParsePrefix("192.0.0.0/24"),  // IETF protocol assignments
	netip.MustParsePrefix("198.18.0.0/15"), // benchmarking
	netip.MustParsePrefix("255.255.255.255/32"),
	netip.MustParsePrefix("192.0.2.0/24"),    // TEST-NET-1 documentation (RFC5737)
	netip.MustParsePrefix("198.51.100.0/24"), // TEST-NET-2 documentation (RFC5737)
	netip.MustParsePrefix("203.0.113.0/24"),  // TEST-NET-3 documentation (RFC5737)
	netip.MustParsePrefix("192.88.99.0/24"),  // deprecated 6to4 relay anycast (RFC7526)
	netip.MustParsePrefix("240.0.0.0/4"),     // reserved / class E (RFC1112)
}

// ReservedV6 are the reserved IPv6 ranges every guard denies that no stdlib predicate covers (IsPrivate
// covers fc00::/7, IsLinkLocalUnicast covers fe80::/10). Same canonical surface as ReservedV4.
//
// 2002::/16 is denied wholesale here, not parsed in NAT64Prefixes: it embeds a real IPv4 at bits 16..48,
// not the low 32 NAT64EmbeddedV4 extracts, and 6to4 is deprecated and unroutable anyway.
var ReservedV6 = []netip.Prefix{
	netip.MustParsePrefix("fec0::/10"), // deprecated site-local (RFC3879) — still configured on some enterprise LANs
	netip.MustParsePrefix("2002::/16"), // 6to4 (RFC7526, deprecated) — embeds an IPv4 at bits 16..48
}

// NAT64Prefixes are the well-known + local-use NAT64 translation prefixes (RFC 6052 / RFC 8215).
// SECURITY: an address inside one carries a real IPv4 in its low 32 bits, so a private/metadata target can
// be smuggled as an IPv6 literal (64:ff9b::a9fe:a9fe -> 169.254.169.254) past every stdlib predicate —
// every guard must block these wholesale and re-check the embedded v4.
var NAT64Prefixes = []netip.Prefix{
	netip.MustParsePrefix("64:ff9b::/96"),   // well-known NAT64 (RFC 6052)
	netip.MustParsePrefix("64:ff9b:1::/48"), // local-use NAT64 (RFC 8215)
}

// PrivateReserved reports whether ip is in a private/reserved range every guard denies regardless of mode
// (RFC1918 + IPv6 ULA via stdlib IsPrivate, plus ReservedV4/ReservedV6), naming the matching range for the
// denial reason. IPv4-mapped IPv6 addresses are unwrapped first.
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
	if i := slices.IndexFunc(ReservedV6, func(p netip.Prefix) bool { return p.Contains(addr) }); i >= 0 {
		return true, ReservedV6[i].String()
	}
	return false, ""
}

// Liftable is the private/reserved space an operator may declare an internal hostname's SSRF-guard
// exception into (SiteConfig's InternalHosts). SECURITY invariant: deliberately narrower than
// PrivateReserved's full denial set — RFC1918, IPv6 ULA, and CGNAT only. Loopback/link-local/metadata/
// unspecified/multicast/NAT64-embedded stay un-liftable by construction.
var Liftable = []netip.Prefix{
	netip.MustParsePrefix("10.0.0.0/8"),
	netip.MustParsePrefix("172.16.0.0/12"),
	netip.MustParsePrefix("192.168.0.0/16"),
	netip.MustParsePrefix("fc00::/7"),
	netip.MustParsePrefix("100.64.0.0/10"), // CGNAT (RFC6598)
}

// InLiftable reports whether ip falls inside the Liftable set.
func InLiftable(ip net.IP) bool {
	addr, ok := addrOf(ip)
	if !ok {
		return false
	}
	return slices.ContainsFunc(Liftable, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// NAT64EmbeddedV4 returns the IPv4 embedded in ip's low 32 bits and true when ip is inside a NAT64 prefix,
// else (nil, false). SECURITY: callers must block the prefix wholesale and re-run the embedded v4 through
// their own v4 guard, so a NAT64-smuggled private/metadata target can't slip past To4()==nil.
func NAT64EmbeddedV4(ip net.IP) (net.IP, bool) {
	addr, ok := addrOf(ip)
	if !ok || !slices.ContainsFunc(NAT64Prefixes, func(p netip.Prefix) bool { return p.Contains(addr) }) {
		return nil, false
	}
	b := addr.As16()
	return net.IP(b[12:16]), true
}

// GatewayIPRefused reports whether ip is a kind an operator-configured internal model gateway can never
// legitimately be: loopback, link-local (metadata address included), unspecified, multicast, or
// NAT64-embedded. RFC1918/ULA/CGNAT are NOT refused — an internal gateway is expected to live there, so
// this is a DIFFERENT predicate from PrivateReserved, not a subset of it.
//
// TRUST BOUNDARY: shared body for two consumers whose agreement matters — the control plane validates the
// configured gateway at boot, and the proxy re-checks the RESOLVED answer per request before dialling it
// with the brokered model credential.
//
// A nil/!ok address is REFUSED: addrOf fails only on a slice that isn't an address at all, and an
// unparseable gateway address is not one this may admit.
func GatewayIPRefused(ip net.IP) bool {
	if _, ok := addrOf(ip); !ok {
		return true
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	_, isNAT64 := NAT64EmbeddedV4(ip)
	return isNAT64
}

// addrOf converts to a comparable netip.Addr, unmapping IPv4-mapped IPv6 so "::ffff:10.0.0.1" is tested
// as the IPv4 address it is.
func addrOf(ip net.IP) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	return addr.Unmap(), ok
}
