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
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"unicode"

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
//
// A run with no network (Config.NoNetwork) is refused here first, for every host. After the blocked-range
// checks, a host with a character outside ASCII is refused on every run (nonASCIIHost), and so is every
// other non-standard spelling of an address (nonStandardIPSpelling).
// A policy with refuse_ip_literals under allow_all_egress then refuses every standard literal too; an
// address listed exactly in the allowlist (Policy.AllowsLiteralIP) is still allowed. Under default-deny the
// switch does nothing: an unlisted literal is the allowlist's to deny, or first-use approval's to hold.
func (p *Proxy) literalIPGuard(req egress.Request, host string, port int) (net.IP, *egress.DecisionLog) {
	if p.noNetwork {
		log := decisionLog(req, egress.Deny, ruleSourceNoNetwork)
		return nil, &log
	}
	deny := func() (net.IP, *egress.DecisionLog) {
		log := decisionLog(req, egress.Deny, "builtin:private-ip")
		return nil, &log
	}
	trusted, denied := p.blockedLiteralIPGuard(deny, host, port)
	if denied != nil || trusted != nil {
		return trusted, denied
	}
	if nonASCIIHost(host) {
		log := decisionLog(req, egress.Deny, ruleSourceNonASCIIHost)
		return nil, &log
	}
	if nonStandardIPSpelling(host) {
		log := decisionLog(req, egress.Deny, ruleSourceIPSpelling)
		return nil, &log
	}
	if p.policy == nil || !p.policy.refuseIPLiterals || !p.policy.allowAll {
		return nil, nil
	}
	if ip := net.ParseIP(trimLiteralHost(host)); ip != nil && !p.policy.AllowsLiteralIP(ip.String(), port) {
		log := decisionLog(req, egress.Deny, ruleSourceIPLiteral)
		return nil, &log
	}
	return nil, nil
}

// The rule sources literalIPGuard writes beside builtin:private-ip.
const (
	ruleSourceNoNetwork    = "policy:no-network"
	ruleSourceIPLiteral    = "policy:ip-literal"
	ruleSourceIPSpelling   = "builtin:ip-spelling"
	ruleSourceNonASCIIHost = "builtin:non-ascii-host"
)

// The 403 details of the two spelling refusals. Fixed text: neither echoes the host.
const (
	ipSpellingDetail = "this host is written as numbers, as an IP address in a non-standard form, or with a colon or a percent sign, " +
		"which the proxy refuses on every run whatever the policy allows, because programs differ on which address such a spelling names. " +
		"Write an address in standard form: four decimal numbers from 0 to 255 with no leading zeros, or an IPv6 address with no zone."
	nonASCIIHostDetail = "this host has a character outside ASCII, which the proxy refuses on every run whatever the policy allows, " +
		"because programs differ on which name or address such a host names. " +
		"Send the name in its ASCII form: an internationalised name as its xn-- form, an IP address in standard form."
)

// nonASCIIHost reports whether host has a character outside ASCII. A client sends an internationalised
// name as its xn-- form; a raw one is mapped differently by different programs (one maps a full-width
// digit or an ideographic full stop, another drops a joiner, a third maps nothing), so no check of the
// host as written binds what the hop that resolves it reads: example.com in full-width letters is
// example.com to one and another name to the next, and 8.8.8.8 followed by an ideographic full stop is an
// address to one and a name to the next.
func nonASCIIHost(host string) bool {
	return strings.ContainsFunc(host, func(r rune) bool { return r > unicode.MaxASCII })
}

// nonStandardIPSpelling reports whether an ASCII host writes an address in any spelling but the one
// net.ParseIP reads: every dot-separated part a number (decimal, octal or 0x hex, an empty 0x being 0), or
// a colon or percent sign, which no host name holds. A non-ASCII host is nonASCIIHost's, which step 0 and
// ValidDomainEntry's charset rule both check first. Programs disagree on which address such a spelling
// names (010.8.8.8 is 8.8.8.8 to inet_aton and 10.8.8.8 to a decimal parser; 008.8.8.8 and 0x are an
// address to some and not to others), so no check of one decoded address binds what the hop that dials
// it reads, and the spelling itself is refused.
func nonStandardIPSpelling(host string) bool {
	h := strings.TrimRight(strings.ToLower(trimLiteralHost(host)), ".")
	if h == "" || net.ParseIP(h) != nil {
		return false
	}
	if strings.ContainsAny(h, ":%") {
		return true
	}
	for part := range strings.SplitSeq(h, ".") {
		digits, base := part, "0123456789"
		if rest, ok := strings.CutPrefix(part, "0x"); ok {
			digits, base = rest, "0123456789abcdef"
		} else if part == "" {
			return false
		}
		if strings.Trim(digits, base) != "" {
			return false
		}
	}
	return true
}

// refuseNoNetwork writes a run with no network's refusal of a request that never reaches evaluate.
func (p *Proxy) refuseNoNetwork(w http.ResponseWriter, r *http.Request, authority string) {
	host, port := splitHostPort(authority, 443)
	log := decisionLog(egress.Request{AuditReservation: spoolTicket(r.Context()), RunID: p.runID, Host: host, Port: port,
		Method: strings.ToUpper(r.Method), Time: p.now()}, egress.Deny, ruleSourceNoNetwork)
	p.sink.emit(log)
	p.writeEgressDeny(w, host, port, &log, false)
}

// errNoNetwork is the egress dial's refusal on a run with no network.
var errNoNetwork = errors.New("proxy: this run has no network")

// noNetworkDial wraps the sandbox's egress dial so a run with no network dials nothing, a backstop
// behind literalIPGuard. directDial and p.dial are never wrapped: the control-plane transport shares
// them, so the run still reaches its control plane.
func (p *Proxy) noNetworkDial(dial func(context.Context, string, string) (net.Conn, error)) func(context.Context, string, string) (net.Conn, error) {
	if dial == nil {
		return nil
	}
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if p.noNetwork {
			return nil, errNoNetwork
		}
		return dial(ctx, network, addr)
	}
}

// trimLiteralHost is host lower-cased, without a trailing dot or the brackets of an IPv6 authority.
func trimLiteralHost(host string) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if strings.HasPrefix(h, "[") && strings.HasSuffix(h, "]") {
		h = h[1 : len(h)-1]
	}
	return h
}

// literalHostIP is the address host names when it is an IP literal in any spelling net.ParseIP or a
// resolver accepts (nonCanonicalLiteralIP), nil for a hostname.
func literalHostIP(host string) net.IP {
	h := trimLiteralHost(host)
	if ip := net.ParseIP(h); ip != nil {
		return ip
	}
	return nonCanonicalLiteralIP(h)
}

// blockedLiteralIPGuard is the blocked-range half of literalIPGuard: a literal in a blocked range is
// denied unless it is a trusted exact entry, which it returns.
func (p *Proxy) blockedLiteralIPGuard(deny func() (net.IP, *egress.DecisionLog), host string, port int) (net.IP, *egress.DecisionLog) {
	if ip := net.ParseIP(strings.TrimSuffix(strings.ToLower(host), ".")); ip != nil {
		// A k8s runner's hard deny beats the exact-literal trust below, as it beats every admission in
		// egressTarget: no allow, approval or redirect reopens it. A trusted literal skips egressTarget,
		// so it is checked here.
		if p.hardDeny.ip(ip) {
			return deny()
		}
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
		if kind, _ := isBlockedIP(ip); kind != blockNone || p.hardDeny.ip(ip) {
			return deny()
		}
	}
	return nil, nil
}

// nonCanonicalLiteralIP is the single gap-filler beside an existing net.ParseIP check: the address host
// names in a spelling net.ParseIP REFUSES and something downstream accepts — inet_aton IPv4 forms
// (nonCanonicalIPv4) and the zone-suffixed IPv6 literal (zonedIPv6Literal). Nil for every spelling
// net.ParseIP already accepts and for an ordinary hostname. SECURITY: every reader denies or words a
// refusal; none grants, so it widens nothing.
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
// only through nonCanonicalLiteralIP, whose readers all deny or word a refusal; none grants — a
// non-canonical spelling can never inherit an allowed_domains grant (authored and matched in canonical form).
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
	// 32 bits: no part, not even a lone one, names more than the whole address.
	v, err := strconv.ParseUint(s, base, 32)
	if err != nil {
		return 0, false
	}
	return v, true
}
