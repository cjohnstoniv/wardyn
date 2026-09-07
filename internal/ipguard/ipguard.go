// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package ipguard holds the SSRF private/reserved membership test shared by the
// egress proxy's policy guard (internal/egress/proxy) and the composer transport
// guard (internal/composer/backends/transport). MEMBERSHIP ONLY, by design: each
// consumer keeps its own predicate around it because their semantics
// deliberately differ. This package carries only what the stdlib does not
// (netip.Addr.IsPrivate covers RFC1918 and the IPv6 ULA range, so those are NOT
// re-listed in ReservedV4 — they ARE re-listed in Liftable, which is a different
// job; see there).
//
// An earlier version of this comment described an "allowPrivate escape hatch"
// belonging to the AI Run Composer's transport. That package was deleted, and
// the stale sentence went on to cost a later design round a wrong conclusion —
// it read as "no operator override exists", when the real override is
// SiteConfig.InternalHosts (see Liftable below). Naming a dead consumer is
// worse than naming none.
package ipguard

import (
	"net"
	"net/netip"
	"slices"
)

// ReservedV4 are the reserved IPv4 ranges every guard denies that
// netip.Addr.IsPrivate does not already cover.
//
// THE CANONICAL SURFACE is the IANA IPv4 Special-Purpose Address Registry, not
// a remembered list of RFCs: an entry belongs here when the registry's
// "Globally Reachable" column says False and no stdlib predicate
// (IsUnspecified/IsLoopback/IsLinkLocalUnicast/IsLinkLocalMulticast/
// IsMulticast/IsPrivate) already covers it. Diff against the registry, not
// against intuition — that is what an audit found missing here: 240.0.0.0/4 was
// absent while 255.255.255.255/32, a /32 INSIDE it, was listed, so the table
// blocked one address of a reserved /4 and left the other ~268 million open,
// and 240/4 is used as extra private space inside some estates, which is
// exactly the class of internal endpoint this guard is the backstop for.
// Registry rows that ARE globally reachable (192.31.196.0/24 AS112-v4,
// 192.52.193.0/24 AMT, 192.175.48.0/24 direct-delegation AS112) stay out.
//
// ORDER MATTERS for the reason string only: PrivateReserved names the FIRST
// matching prefix, so 255.255.255.255/32 stays listed ahead of the 240.0.0.0/4
// that now contains it and keeps naming itself in a denial.
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

// ReservedV6 are the reserved IPv6 ranges every guard denies that no stdlib
// predicate covers (netip.Addr.IsPrivate covers fc00::/7, IsLinkLocalUnicast
// covers fe80::/10, so neither is re-listed). Same canonical surface as
// ReservedV4: the IANA IPv6 Special-Purpose Address Registry.
//
// 2002::/16 is here rather than in NAT64Prefixes on purpose. It carries a real
// IPv4 exactly as a NAT64 prefix does — 2002:7f00:0001::1 is 127.0.0.1 — but at
// bits 16..48 rather than in the low 32, so NAT64EmbeddedV4's extraction cannot
// name the target. 6to4 is deprecated and unroutable (RFC7526), so the whole
// prefix is denied wholesale instead: strictly stronger than an extraction, and
// it leaves ONE place to look for "which embedded-v4 shapes are covered".
var ReservedV6 = []netip.Prefix{
	netip.MustParsePrefix("fec0::/10"), // deprecated site-local (RFC3879) — still configured on some enterprise LANs
	netip.MustParsePrefix("2002::/16"), // 6to4 (RFC7526, deprecated) — embeds an IPv4 at bits 16..48
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
// IsPrivate) plus ReservedV4 and ReservedV6 — and names the matching range for
// the denial reason. IPv4-mapped IPv6 addresses are unwrapped first.
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

// Liftable is the private/reserved-address space an operator may declare a
// specific internal hostname's SSRF-guard exception into (SiteConfig's
// InternalHosts, and the proxy's per-request internal-host lift it drives).
// Deliberately narrower than PrivateReserved's full denial set: RFC1918, the
// IPv6 ULA range, and CGNAT only. Loopback/link-local/metadata/unspecified/
// multicast/NAT64-embedded stay un-liftable by construction — the proxy's
// blockKind classification (internal/egress/proxy) never offers them to the
// lift predicate in the first place, and the site-config write validator
// checks a declared CIDR against exactly this set.
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

// GatewayIPRefused reports whether ip is a kind an operator-configured internal
// model gateway can never legitimately be: loopback, link-local (the metadata
// address included), unspecified, multicast, or NAT64-embedded. RFC1918/ULA/
// CGNAT are NOT refused — those are exactly the addresses an internal gateway is
// expected to live on, which is what makes this a DIFFERENT predicate from
// PrivateReserved rather than a subset of it.
//
// It lives here because it has two consumers whose agreement is a trust
// boundary and used to be a comment: the control plane validates the operator's
// configured gateway at boot (internal/api), and the proxy re-checks the
// RESOLVED answer per request before dialling it with the brokered model
// credential (internal/egress/proxy). Those were byte-identical copies coupled
// only by a "mirrors …" sentence, and the proxy copy's NAT64 arm was unpinned:
// deleting it left the whole proxy suite green while a gateway name resolving to
// 64:ff9b::a9fe:a9fe (NAT64-mapped 169.254.169.254) became dialable with the
// credential. One body, one table test, no drift.
//
// A nil/!ok address is REFUSED: addrOf fails only on a slice that is not an
// address at all, and an unparseable gateway address is not one this may admit.
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

// addrOf converts a net.IP to a comparable netip.Addr, unmapping IPv4-mapped
// IPv6 so "::ffff:10.0.0.1" is tested as the IPv4 address it is.
func addrOf(ip net.IP) (netip.Addr, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	return addr.Unmap(), ok
}
