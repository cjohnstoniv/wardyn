// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// SECURITY: the unconditional literal-IP guard evaluate applies at step 0 — INCLUDING the non-canonical
// spellings a resolver accepts and net.ParseIP does not.
//
// net.ParseIP is incomplete on TWO axes: (1) inet_aton(3) — and so glibc getaddrinfo and most libc-linked
// upstreams — accepts dotted-triple, dotted-double, bare 32-bit, hex and octal spellings net.ParseIP
// refuses (127.1, 0x7f000001, 2130706433 all read as 127.0.0.1; 0251.0376.0.1 reads as 169.254.0.1, a
// DIFFERENT blocked range); (2) net.ParseIP returns nil for a ZONE-SUFFIXED IPv6 literal ("fe80::1%eth0")
// that netip.ParseAddr parses and net.Dial dials.
//
// Left unvetted, those spellings walk past step 0 as ordinary "hostnames" and, with a corp upstream
// configured, are handed over verbatim — SSRF to loopback/metadata through the one hop the threat model
// says the guard still covers.

import (
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// literalIPGuard is evaluate's step 0. INVARIANT 3: an agent-named LITERAL address that is blocked
// (private/loopback/link-local/metadata) is denied BEFORE policy and BEFORE first-use approval, so an
// approval can never be raisable for these ranges. Hostnames that RESOLVE to blocked ranges are caught by
// VetHost at step 4, after policy/approval. Returns (trustedLiteralIP, nil) to continue, (nil, log) to deny.
//
// trustedLiteralIP is the ONE deliberate exception: a literal IP the operator explicitly typed into an
// EXACT AllowedDomains entry carries no DNS-rebinding risk (no hostname behind it to rebind), so it also
// skips VetHost. Only blockPrivate OFF the proxy's own subnets and control-plane host qualifies — loopback,
// link-local/metadata, NAT64, or a sidecar docker-network neighbour is still denied regardless.
//
// The non-canonical arm is DENY-ONLY (see nonCanonicalLiteralIP): a spelling the operator didn't type can
// never inherit an allowed_domains grant.
func (p *Proxy) literalIPGuard(req egress.Request, host string, port int) (net.IP, *egress.DecisionLog) {
	deny := func() (net.IP, *egress.DecisionLog) {
		log := decisionLog(req, egress.Deny, "builtin:private-ip")
		return nil, &log
	}
	if ip := net.ParseIP(strings.TrimSuffix(strings.ToLower(host), ".")); ip != nil {
		if kind, _ := isBlockedIP(ip); kind != blockNone {
			if !p.trustsExactLiteralIP(ip, port) {
				return deny()
			}
			return ip, nil
		}
		return nil, nil
	}
	// Spellings net.ParseIP refuses but a real dialer accepts (see package comment) — what finally dials
	// sees the address the canonical spelling names, so the guard must too.
	if ip := nonCanonicalLiteralIP(host); ip != nil {
		if kind, _ := isBlockedIP(ip); kind != blockNone {
			return deny()
		}
	}
	return nil, nil
}

// nonCanonicalLiteralIP is the single gap-filler beside an existing net.ParseIP check: the address host
// names in a spelling net.ParseIP REFUSES and something downstream accepts — inet_aton IPv4 forms
// (nonCanonicalIPv4) and the zone-suffixed IPv6 literal (zonedIPv6Literal). Nil for every spelling
// net.ParseIP already accepts and for an ordinary hostname. SECURITY: both consumers — evaluate's step 0
// and egressTarget's upstream branch — DENY on it; nothing else reads it, so it widens nothing.
func nonCanonicalLiteralIP(host string) net.IP {
	if ip := nonCanonicalIPv4(host); ip != nil {
		return ip
	}
	return zonedIPv6Literal(host)
}

// zonedIPv6Literal returns the address a ZONE-SUFFIXED IPv6 literal names — "fe80::1%eth0", and the RFC
// 6874 authority spelling "fe80::1%25eth0" a URI carries it in — nil otherwise. net.ParseIP has no zone
// syntax and returns nil for both, while netip.ParseAddr parses and net.Dial dials them, so that's the
// parser this guard must agree with.
//
// The zone is dropped from the returned address: a zone selects which INTERFACE is reached, never a
// different address, and isBlockedIP judges the address. Dropping it also keeps this arm deny-only.
func zonedIPv6Literal(host string) net.IP {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || net.ParseIP(h) != nil {
		return nil
	}
	if i := strings.Index(h, "%25"); i >= 0 {
		h = h[:i] + "%" + h[i+len("%25"):]
	}
	addr, err := netip.ParseAddr(h)
	if err != nil || addr.Zone() == "" {
		return nil
	}
	return net.IP(addr.WithZone("").AsSlice())
}

// nonCanonicalIPv4 returns the IPv4 address host names under inet_aton(3) semantics when host is a numeric
// literal net.ParseIP REFUSES, nil otherwise (including every spelling net.ParseIP already accepts). Read
// only through nonCanonicalLiteralIP, whose two consumers deny — a non-canonical spelling can never
// inherit an allowed_domains grant (authored and matched in canonical form).
func nonCanonicalIPv4(host string) net.IP {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" || net.ParseIP(h) != nil {
		return nil
	}
	parts := strings.Split(h, ".")
	if len(parts) > 4 {
		return nil
	}
	vals := make([]uint64, 0, 4)
	for _, part := range parts {
		v, ok := inetAtonPart(part)
		if !ok {
			return nil
		}
		vals = append(vals, v)
	}
	// inet_aton: with n parts, the LAST absorbs the remaining 5-n bytes (so "127.1" is 127.0.0.1 and
	// "2130706433" is the whole word); every earlier part must fit in one byte.
	n := len(vals)
	last := vals[n-1]
	if last >= uint64(1)<<(8*(5-n)) {
		return nil
	}
	var addr uint32
	for i := range n - 1 {
		if vals[i] > 0xff {
			return nil
		}
		addr |= uint32(vals[i]) << (8 * (3 - i))
	}
	addr |= uint32(last)
	return net.IPv4(byte(addr>>24), byte(addr>>16), byte(addr>>8), byte(addr))
}

// inetAtonPart parses one inet_aton component: 0x-prefixed hex, 0-prefixed octal, or decimal. Anything
// else makes the whole host an ordinary hostname.
func inetAtonPart(s string) (uint64, bool) {
	base := 10
	switch {
	case strings.HasPrefix(s, "0x"):
		base, s = 16, s[2:]
	case len(s) > 1 && s[0] == '0':
		base, s = 8, s[1:]
	}
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
