// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The unconditional literal-IP guard evaluate applies at step 0 — INCLUDING
// the non-canonical spellings a resolver accepts and net.ParseIP does not
// (F105).
//
// Split out of proxy.go at the 1000-line gate, and a real seam rather than a
// size dodge: the rule, its one deliberate exception, and the spelling
// grammar it has to cover are one subject, and evaluate now states the rule in
// three lines instead of forty.
//
// The unconditional literal-IP guard (evaluate step 0) and every doc that
// states its bound — "the destination is named by HOSTNAME: a literal
// private/loopback/link-local/metadata IP is still denied at the literal-IP
// guard" (threatmodel/THREAT-MODEL.md) — are written as though "is this a
// literal IP?" were answered by net.ParseIP. It is not, on TWO axes.
//
// (1) inet_aton(3) — and therefore glibc getaddrinfo, and therefore essentially
// every corporate forward proxy and every libc-linked upstream — accepts the
// dotted-triple, dotted-double, bare 32-bit, hexadecimal and octal spellings of
// an address net.ParseIP refuses (Go accepts ONLY the canonical dotted-quad).
// 127.1, 0x7f000001 and 2130706433 all read as 127.0.0.1 to the thing that
// finally dials; 0251.0376.0.1 reads as 169.254.0.1 — a link-local address, NOT
// loopback (the pairing matters: the guard blocks both, the doc must not claim
// the wrong one).
//
// (2) net.ParseIP returns nil for a ZONE-SUFFIXED IPv6 literal ("fe80::1%eth0",
// and the RFC 6874 authority spelling "fe80::1%25eth0" a URI carries it in),
// while netip.ParseAddr parses it and net.Dial dials it. The canonical spelling
// of that same address is denied at step 0, so the zone id alone used to decide
// the verdict.
//
// Left unvetted those spellings walked straight past step 0 as ordinary
// "hostnames" and, with a corp upstream configured, were handed to the
// operator's proxy verbatim — SSRF to loopback/metadata through the one hop
// the threat model says the guard still covers.

import (
	"net"
	"net/netip"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
)

// literalIPGuard is evaluate's step 0: an agent-named LITERAL address that is
// blocked (private/loopback/link-local/metadata) is denied BEFORE policy and
// BEFORE first-use approval — an approval must never even be raisable for
// these ranges (invariant 3: blocked regardless of policy). Hostnames that
// RESOLVE to blocked ranges are caught by VetHost at step 4, after
// policy/approval.
//
// It returns (trustedLiteralIP, nil) to continue and (nil, log) to deny.
//
// trustedLiteralIP is the ONE deliberate exception: a literal IP the operator
// explicitly typed into an EXACT AllowedDomains entry (e.g. an egress-redirect
// "To" target on RFC1918/CGNAT space — see trustsExactLiteralIP) carries no
// DNS-rebinding risk, since there is no hostname behind it to rebind. Returned
// non-nil it also skips VetHost at step 4 (which would otherwise re-derive and
// re-deny the same address), so the operator's own configured destination is
// actually reachable instead of always denied with "the customer's network is
// at fault" (W13-S1-3). Only blockPrivate OFF the proxy's own subnets and
// control-plane host qualifies: a declared loopback, link-local/metadata or
// NAT64 literal — or one of the sidecar's own docker-network neighbours — is
// still denied, so the operator can hand the sandbox neither 169.254.169.254
// nor the control plane by allow-listing it.
//
// The non-canonical arm is DENY-ONLY (see nonCanonicalLiteralIP): a spelling
// the operator did not type is never offered to trustsExactLiteralIP, so it
// can inherit no allowed_domains grant, and an unblocked one falls through to
// policy exactly as it did before.
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
	// The spellings net.ParseIP refuses and a real dialer accepts: the
	// inet_aton readings (127.1 / 0x7f000001 / 2130706433 are 127.0.0.1 to glibc
	// getaddrinfo, 0251.0376.0.1 is 169.254.0.1) and the zone-suffixed IPv6
	// literal net.Dial dials (fe80::1%eth0). Either way what finally dials sees
	// the address the canonical spelling names, so the guard has to as well.
	if ip := nonCanonicalLiteralIP(host); ip != nil {
		if kind, _ := isBlockedIP(ip); kind != blockNone {
			return deny()
		}
	}
	return nil, nil
}

// nonCanonicalLiteralIP is the single gap-filler beside an existing net.ParseIP
// check: the address host names in a spelling net.ParseIP REFUSES and something
// downstream accepts — the inet_aton IPv4 forms (nonCanonicalIPv4) and the
// zone-suffixed IPv6 literal (zonedIPv6Literal). It is nil for every spelling
// net.ParseIP already accepts and for an ordinary hostname, so it can never
// change what the canonical path decides.
//
// TRUST BOUNDARY: this widens nothing. Both consumers — evaluate's step 0 and
// egressTarget's upstream branch — DENY on it and nothing else reads it, so a
// spelling the operator did not type is never offered to trustsExactLiteralIP
// or to any allow path.
func nonCanonicalLiteralIP(host string) net.IP {
	if ip := nonCanonicalIPv4(host); ip != nil {
		return ip
	}
	return zonedIPv6Literal(host)
}

// zonedIPv6Literal returns the address a ZONE-SUFFIXED IPv6 literal names —
// "fe80::1%eth0", and the RFC 6874 authority spelling "fe80::1%25eth0" that a
// URI (and therefore a CONNECT authority) carries the zone id in — and nil for
// anything else.
//
// net.ParseIP has no zone syntax and returns nil for both, so the literal-IP
// guard read them as ordinary hostnames: "fe80::1" denied at step 0 with
// builtin:private-ip while "fe80::1%eth0" went to policy, could raise a
// first-use approval for a link-local address (which invariant 3 says must
// never be raisable), and under a corp upstream was handed over verbatim.
// netip.ParseAddr does parse them and net.Dial does dial them, so that is the
// parser this guard has to agree with.
//
// The zone is dropped from the returned address on purpose: a zone selects
// which INTERFACE an address is reached on, never a different address, and
// isBlockedIP judges the address. Dropping it also keeps this arm deny-only —
// the value never reaches trustsExactLiteralIP.
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

// nonCanonicalIPv4 returns the IPv4 address host names under inet_aton(3)
// semantics when host is a numeric literal that net.ParseIP REFUSES, and nil
// otherwise — including for every spelling net.ParseIP already accepts, so it
// is purely the gap-filler beside an existing net.ParseIP check and can never
// change what the canonical path does.
//
// TRUST BOUNDARY: this widens nothing. It is read only through
// nonCanonicalLiteralIP, whose two consumers deny; a non-canonical spelling is
// never offered to trustsExactLiteralIP or to any allow path, because an
// operator authors allowed_domains entries in canonical form (AllowsLiteralIP
// keys on net.IP.String()) and a spelling an operator did not type must not be
// able to inherit an operator's grant.
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
	// inet_aton: with n parts, the LAST absorbs the remaining 5-n bytes of the
	// address (so "127.1" is 127.0.0.1 and "2130706433" is the whole word) and
	// every earlier part must fit in one byte.
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

// inetAtonPart parses one component of an inet_aton literal: 0x-prefixed
// hexadecimal, 0-prefixed octal, or decimal — the exact grammar inet_aton(3)
// documents. Anything else (a letter, an empty component, a bare "0x") is not
// a numeric literal at all and makes the whole host an ordinary hostname.
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
