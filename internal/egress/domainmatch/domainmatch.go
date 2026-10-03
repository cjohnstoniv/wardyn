// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package domainmatch is the ONE reading of an allowed_domains/denied_domains
// entry: how an entry is spelled (Classify), what spelling a request host is
// keyed on (CanonHost), and when a host falls under a wildcard (MatchWild,
// MatchWildPort).
//
// The egress proxy compiles its policy through these functions, and the
// governance composer decides whether one entry is covered by another through
// the same ones, so "covered by the base" can never mean something the proxy
// would not enforce. It is a leaf (standard library only): the composer must
// not take the proxy's TLS, content-scan, git-pack and SSRF-guard code as
// dependencies for the sake of a string matcher.
package domainmatch

import (
	"net"
	"strconv"
	"strings"
)

// WildPort is a port-qualified wildcard entry: suffix WITHOUT the leading "*"
// (e.g. ".example.com") that matches only when the request port equals Port.
type WildPort struct {
	Suffix string
	Port   int
}

// CanonHost is the ONE spelling every policy map is keyed on, at compile time
// and at lookup: lower-cased, trailing dot trimmed, and — when the string is an
// IP LITERAL — the canonical net.IP.String() form of it.
//
// The literal half closes a real deny bypass, not a cosmetic inconsistency:
// evalHost keyed on the raw request string while AllowsLiteralIP keyed on
// ip.String(), so a run that denies 93.184.216.34 still allowed
// "::ffff:93.184.216.34" — which vetHostLift's literal fast path parses back
// to the same address and dials, the whole barrier gone under allow_all_egress.
// Literal-IP deny entries are first-class here: ValidDomainEntry exempts IPv6
// literals from the ":port" check, and literalIPDenialDetail composes an
// operator message about them.
//
// The same normalization on the ENTRY side (Classify) closes the other
// half: a non-canonical spelling in denied_domains was a dead entry protecting
// nothing. Both sides now land in the same space and can no longer disagree.
//
// It does NOT widen an allow to a different destination: deny lookups are
// canonicalized in the same call and still run first, and a spelling
// net.ParseIP cannot read ("127.1", a zone-suffixed "fe80::1%eth0") is left
// verbatim — it matches no allow entry, and the unconditional IP guard still
// binds the dial.
func CanonHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
}

// Classify normalizes a configured domain entry. A "*.example.com"
// pattern yields a wildcard suffix ".example.com" (label-boundary match);
// anything else is an exact host. An optional ":port" qualifier ("host:443",
// "*.example.com:443") is parsed out and returned as port>0; a bare entry
// returns port==0 and matches ANY port.
func Classify(d string) (exact, wild string, port int) {
	d = strings.ToLower(strings.TrimSpace(d))
	// TrimRight, not TrimSuffix: the REQUEST side normalises every FQDN-root
	// spelling with TrimRight (splitHostPort), so an entry trimming only one
	// dot compiled to a key no request host equals. Root cause for allow and
	// deny at once, since both compile through here.
	d = strings.TrimRight(d, ".")
	if d == "" {
		return "", "", 0
	}
	// Optional :port qualifier. Only a VALID port (1..65535) is honored; a
	// non-numeric or out-of-range suffix is left attached, so the entry stays
	// an exact host that never matches a real request — it must NOT silently
	// degrade to a bare any-port match, which would widen egress.
	if h, ps, err := net.SplitHostPort(d); err == nil {
		if n, perr := strconv.Atoi(ps); perr == nil && n >= 1 && n <= 65535 {
			d = h
			port = n
		}
	}
	if strings.HasPrefix(d, "*.") {
		// ".example.com" — suffix-match on the label boundary.
		return "", d[1:], port
	}
	// CanonHost, not the raw string: an entry spelled as an alternate IPv6
	// form must compile to the same key the request side derives, or the
	// entry is dead. No wildcard IP form, so only this branch carries a literal.
	return CanonHost(d), "", port
}

// MatchWild reports whether host falls under any wildcard suffix. A suffix
// ".example.com" matches "a.example.com" and "x.y.example.com" but NOT
// "example.com" itself nor "notexample.com" (label-boundary safe).
func MatchWild(host string, wilds []string) bool {
	for _, w := range wilds {
		if strings.HasSuffix(host, w) {
			return true
		}
	}
	return false
}

// MatchWildPort is MatchWild for port-qualified wildcard entries: the suffix
// must match AND the request port must equal the entry's port.
func MatchWildPort(host string, port int, wilds []WildPort) bool {
	for _, w := range wilds {
		if w.Port == port && strings.HasSuffix(host, w.Suffix) {
			return true
		}
	}
	return false
}
