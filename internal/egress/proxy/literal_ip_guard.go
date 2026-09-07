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
// literal IP?" were answered by net.ParseIP. It is not. net.ParseIP accepts
// ONLY the canonical dotted-quad (Go deliberately rejects the rest), while
// inet_aton(3) — and therefore glibc getaddrinfo, and therefore essentially
// every corporate forward proxy and every libc-linked upstream — also accepts
// the dotted-triple, dotted-double, bare 32-bit, hexadecimal and octal
// spellings of the SAME address. 127.1, 0x7f000001, 2130706433 and
// 0251.0376.0.1 are all 127.0.0.1 to the thing that finally dials.
//
// Left unvetted those spellings walked straight past step 0 as ordinary
// "hostnames" and, with a corp upstream configured, were handed to the
// operator's proxy verbatim — SSRF to loopback/metadata through the one hop
// the threat model says the guard still covers.

import (
	"net"
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
// The non-canonical arm is DENY-ONLY (see nonCanonicalIPv4): a spelling the
// operator did not type is never offered to trustsExactLiteralIP, so it can
// inherit no allowed_domains grant, and an unblocked one falls through to
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
	// The spellings net.ParseIP refuses and inet_aton accepts — 127.1 /
	// 0x7f000001 / 2130706433 / 0251.0376.0.1 are all 127.0.0.1 to glibc
	// getaddrinfo, and therefore to the corp upstream we would otherwise hand
	// the string to verbatim.
	if ip := nonCanonicalIPv4(host); ip != nil {
		if kind, _ := isBlockedIP(ip); kind != blockNone {
			return deny()
		}
	}
	return nil, nil
}

// nonCanonicalIPv4 returns the IPv4 address host names under inet_aton(3)
// semantics when host is a numeric literal that net.ParseIP REFUSES, and nil
// otherwise — including for every spelling net.ParseIP already accepts, so it
// is purely the gap-filler beside an existing net.ParseIP check and can never
// change what the canonical path does.
//
// TRUST BOUNDARY: this widens nothing. Its only consumers deny (evaluate's
// step 0 and egressTarget's upstream branch); a non-canonical spelling is
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
