// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package proxy implements the L2 per-workspace egress sidecar (wardyn-proxy):
// an HTTP forward proxy that enforces the internal/egress decision model
// (default-deny domain allowlist, method rules, first-use approval),
// streams decision logs, and injects credentials proxy-side.
//
// SECURITY INVARIANTS (mirror ARCHITECTURE.md and internal/egress):
//   - Default deny: an empty allowlist allows nothing.
//   - DeniedDomains always beats AllowedDomains.
//   - Private/loopback/link-local/metadata IP ranges are unconditionally
//     denied BEFORE any dial, regardless of policy (DNS-rebinding / SSRF
//     guard). The vetted IP is dialed explicitly — the transport never
//     re-resolves the hostname (no TOCTOU).
//   - Fail closed on every error path.
package proxy

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// hostDecision is the policy-only verdict for a host (before first-use
// approval and before IP vetting). It deliberately excludes Pending: that
// outcome is decided by the approval layer, not pure policy.
type hostDecision string

const (
	hostAllow   hostDecision = "allow"
	hostDeny    hostDecision = "deny"
	hostUnknown hostDecision = "unknown" // not denied, not allowed -> candidate for first-use approval
)

// Policy is a compiled, immutable view of types.RunPolicySpec optimized for
// per-request evaluation.
type Policy struct {
	allowedExact map[string]struct{}
	allowedWild  []string // suffixes WITHOUT the leading "*", i.e. ".example.com"
	deniedExact  map[string]struct{}
	deniedWild   []string
	// Port-qualified variants: an entry "host:port" (or "*.suffix:port") matches
	// ONLY that host+port. A bare host/suffix entry above matches ANY port. Keyed
	// "host:port" (see hostPortKey) for exact, {suffix,port} for wildcard.
	allowedExactPort map[string]struct{}
	allowedWildPort  []wildPort
	deniedExactPort  map[string]struct{}
	deniedWildPort   []wildPort
	allowedMeth      map[string]struct{} // empty == all methods allowed
	firstUse         types.FirstUseMode
	// allowAll switches evalHost from default-deny (allowlist only) to "allow
	// all (deny-list only)": a non-denied host resolves to hostAllow even when
	// it is not in allowedExact/allowedWild. Deny still beats allow, the
	// unconditional VetHost/isBlockedIP private-IP guard is unaffected, and
	// AllowedExactHost (credential injection) is unchanged — injection still
	// requires an explicit exact allowlist entry even under allow-all.
	allowAll bool
}

// CompilePolicy builds a Policy from a RunPolicySpec. Domains are normalized
// to lowercase; trailing dots are stripped. Methods are uppercased.
func CompilePolicy(spec types.RunPolicySpec) *Policy {
	p := &Policy{
		allowedExact:     make(map[string]struct{}),
		deniedExact:      make(map[string]struct{}),
		allowedExactPort: make(map[string]struct{}),
		deniedExactPort:  make(map[string]struct{}),
		allowedMeth:      make(map[string]struct{}),
		firstUse:         spec.FirstUseApproval.Normalize(),
		allowAll:         spec.AllowAllEgress,
	}
	for _, d := range spec.AllowedDomains {
		exact, wild, port := classifyDomain(d)
		switch {
		case wild != "" && port > 0:
			p.allowedWildPort = append(p.allowedWildPort, wildPort{suffix: wild, port: port})
		case wild != "":
			p.allowedWild = append(p.allowedWild, wild)
		case exact != "" && port > 0:
			p.allowedExactPort[hostPortKey(exact, port)] = struct{}{}
		case exact != "":
			p.allowedExact[exact] = struct{}{}
		}
	}
	for _, d := range spec.DeniedDomains {
		exact, wild, port := classifyDomain(d)
		switch {
		case wild != "" && port > 0:
			p.deniedWildPort = append(p.deniedWildPort, wildPort{suffix: wild, port: port})
		case wild != "":
			p.deniedWild = append(p.deniedWild, wild)
		case exact != "" && port > 0:
			p.deniedExactPort[hostPortKey(exact, port)] = struct{}{}
		case exact != "":
			p.deniedExact[exact] = struct{}{}
		}
	}
	for _, m := range spec.AllowedMethods {
		m = strings.ToUpper(strings.TrimSpace(m))
		if m != "" {
			p.allowedMeth[m] = struct{}{}
		}
	}
	return p
}

// FirstUseMode reports how unknown domains are handled (always_deny /
// deny_with_review / wait_for_review), normalized (never empty).
func (p *Policy) FirstUseMode() types.FirstUseMode { return p.firstUse }

// FirstUseApproval reports whether unknown domains escalate to approval (either
// review mode). Retained as a convenience for callers that only need the
// boolean "does this raise an approval" answer.
func (p *Policy) FirstUseApproval() bool { return p.firstUse.RaisesApproval() }

// builtinEvaluator is the default egress.Evaluator: it wraps the compiled
// RunPolicySpec Policy. It decides only the host verdict + method; the proxy
// keeps the IP guard, approval FSM, IP vetting, and injection hardwired.
type builtinEvaluator struct{ p *Policy }

func (b builtinEvaluator) Name() string { return "builtin" }

func (b builtinEvaluator) EvaluateHost(_ context.Context, req egress.Request) (egress.HostVerdict, error) {
	switch b.p.evalHost(req.Host, req.Port) {
	case hostDeny:
		return egress.VerdictDeny, nil
	case hostAllow:
		return egress.VerdictAllow, nil
	default:
		return egress.VerdictUnknown, nil
	}
}

func (b builtinEvaluator) MethodAllowed(method string) bool { return b.p.methodAllowed(method) }

// NewBuiltinEvaluator returns the builtin egress.Evaluator for a compiled spec.
// It is exported so the conformance suite (and operators wiring the builtin
// explicitly) can construct it.
func NewBuiltinEvaluator(spec types.RunPolicySpec) egress.Evaluator {
	return builtinEvaluator{p: CompilePolicy(spec)}
}

// wildPort is a port-qualified wildcard entry: suffix WITHOUT the leading "*"
// (e.g. ".example.com") that matches only when the request port equals port.
type wildPort struct {
	suffix string
	port   int
}

// hostPortKey is the map key for a port-qualified exact entry. It must be built
// identically at compile time and at lookup so "host:443" collides correctly.
func hostPortKey(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

// classifyDomain normalizes a configured domain entry. A "*.example.com"
// pattern yields a wildcard suffix ".example.com" (label-boundary match);
// anything else is an exact host. An optional ":port" qualifier ("host:443",
// "*.example.com:443") is parsed out and returned as port>0; a bare entry
// returns port==0 and matches ANY port.
func classifyDomain(d string) (exact, wild string, port int) {
	d = strings.ToLower(strings.TrimSpace(d))
	d = strings.TrimSuffix(d, ".")
	if d == "" {
		return "", "", 0
	}
	// Optional :port qualifier. Only a VALID port (1..65535) is honored; a
	// non-numeric or out-of-range suffix ("host:0", "host:-1", "host:abc") is
	// left attached, so the entry stays an exact host that never matches a real
	// request host (a malformed entry that simply never matches — it must NOT
	// silently degrade to a bare any-port match, which would widen egress).
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
	return d, "", port
}

// matchWild reports whether host falls under any wildcard suffix. A suffix
// ".example.com" matches "a.example.com" and "x.y.example.com" but NOT
// "example.com" itself nor "notexample.com" (label-boundary safe).
func matchWild(host string, wilds []string) bool {
	for _, w := range wilds {
		if strings.HasSuffix(host, w) {
			return true
		}
	}
	return false
}

// matchWildPort is matchWild for port-qualified wildcard entries: the suffix
// must match AND the request port must equal the entry's port.
func matchWildPort(host string, port int, wilds []wildPort) bool {
	for _, w := range wilds {
		if w.port == port && strings.HasSuffix(host, w.suffix) {
			return true
		}
	}
	return false
}

// evalHost returns the policy-only verdict for a host+port. Deny always wins.
// A bare allow/deny entry matches any port; a port-qualified entry ("host:443")
// matches only that host+port.
func (p *Policy) evalHost(host string, port int) hostDecision {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	key := hostPortKey(host, port)
	// Deny beats allow, unconditionally.
	if _, ok := p.deniedExact[host]; ok {
		return hostDeny
	}
	if _, ok := p.deniedExactPort[key]; ok {
		return hostDeny
	}
	if matchWild(host, p.deniedWild) || matchWildPort(host, port, p.deniedWildPort) {
		return hostDeny
	}
	if _, ok := p.allowedExact[host]; ok {
		return hostAllow
	}
	if _, ok := p.allowedExactPort[key]; ok {
		return hostAllow
	}
	if matchWild(host, p.allowedWild) || matchWildPort(host, port, p.allowedWildPort) {
		return hostAllow
	}
	// Allow-all (deny-list only) mode: any host that survived the deny checks
	// above is allowed. This runs AFTER the deny checks so denied_domains still
	// wins. The unconditional VetHost/isBlockedIP private-IP guard (applied
	// later in the pipeline) is unaffected, so allow-all reaches PUBLIC hosts
	// only. AllowedExactHost (credential injection) deliberately does NOT honor
	// allowAll — injection still requires an explicit exact allowlist entry.
	if p.allowAll {
		return hostAllow
	}
	return hostUnknown
}

// methodAllowed reports whether a plain-HTTP method passes the method
// restriction. CONNECT is treated as a method named "CONNECT". An empty
// restriction set allows all methods.
func (p *Policy) methodAllowed(method string) bool {
	if len(p.allowedMeth) == 0 {
		return true
	}
	_, ok := p.allowedMeth[strings.ToUpper(method)]
	return ok
}

// AllowedExactHost reports whether host is allowed via an EXACT allowlist
// entry (not a wildcard, not approval). Credential injection requires this
// stricter match so an injection rule can never widen egress nor leak a
// secret to a wildcard-matched host.
func (p *Policy) AllowedExactHost(host string) bool {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	if _, ok := p.deniedExact[host]; ok {
		return false
	}
	if matchWild(host, p.deniedWild) {
		return false
	}
	_, ok := p.allowedExact[host]
	return ok
}

// IPGuardResult records the outcome of resolving + vetting a target host.
type IPGuardResult struct {
	// IP is the single vetted address to dial (no further DNS resolution).
	IP net.IP
	// Denied is true when no usable, policy-safe address exists.
	Denied bool
	// Reason explains a denial (for the decision log rule_source).
	Reason string
}

// resolver abstracts DNS for testability.
type resolver interface {
	LookupIP(host string) ([]net.IP, error)
}

// netResolver is the production resolver backed by the stdlib.
type netResolver struct{}

func (netResolver) LookupIP(host string) ([]net.IP, error) {
	return net.LookupIP(host)
}

// VetHost resolves host and returns the first address that is NOT in a
// blocked range. If host is already a literal IP, it is vetted directly.
// On any failure or if every address is blocked, the result is Denied
// (fail closed). The returned IP MUST be the one dialed — callers must not
// re-resolve the hostname (TOCTOU / DNS-rebinding guard).
func VetHost(host string, res resolver) IPGuardResult {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return IPGuardResult{Denied: true, Reason: "empty host"}
	}
	// Literal IP fast path.
	if ip := net.ParseIP(host); ip != nil {
		if blocked, why := isBlockedIP(ip); blocked {
			return IPGuardResult{Denied: true, Reason: why}
		}
		return IPGuardResult{IP: ip}
	}
	if res == nil {
		res = netResolver{}
	}
	ips, err := res.LookupIP(host)
	if err != nil {
		return IPGuardResult{Denied: true, Reason: fmt.Sprintf("resolve failed: %v", err)}
	}
	if len(ips) == 0 {
		return IPGuardResult{Denied: true, Reason: "no addresses"}
	}
	// If ANY resolved address is blocked, deny the whole host: a mix of
	// public and private answers is the classic DNS-rebinding attack shape.
	// Fail closed.
	for _, ip := range ips {
		if blocked, why := isBlockedIP(ip); blocked {
			return IPGuardResult{Denied: true, Reason: fmt.Sprintf("blocked address %s: %s", ip, why)}
		}
	}
	return IPGuardResult{IP: ips[0]}
}

// blockedV4 / blockedV6 are the unconditionally-denied CIDR ranges:
// RFC1918 private space, loopback, link-local, the cloud metadata address
// (169.254.169.254 is inside 169.254.0.0/16), CGNAT, and IPv6 ULA/loopback/
// link-local. These are denied regardless of policy.
var (
	blockedV4 []*net.IPNet
	blockedV6 []*net.IPNet
	// nat64Prefixes are the well-known + local-use NAT64 translation prefixes
	// (RFC 6052 / RFC 8215). An address inside one of these carries a real IPv4
	// in its low 32 bits, so a private/metadata target can be smuggled as an
	// IPv6 literal (64:ff9b::a9fe:a9fe -> 169.254.169.254) that .To4()==nil lets
	// pass every stdlib predicate. Blocked wholesale, and the embedded v4 is
	// re-checked so the denial reason names the real target.
	nat64Prefixes []*net.IPNet
)

func init() {
	// Shared private/reserved core (internal/ipguard) + the proxy-only entries:
	// unlike the composer transport (which spares loopback under its operator
	// allowPrivate escape hatch), the proxy denies loopback/link-local always,
	// so those ranges live in ITS table.
	blockedV4 = append(ipguard.MustCIDRs(
		"127.0.0.0/8",    // loopback
		"169.254.0.0/16", // link-local incl. 169.254.169.254 metadata
	), ipguard.PrivateReservedV4...)
	blockedV6 = append(ipguard.MustCIDRs(
		"::1/128",   // loopback
		"fe80::/10", // link-local
		"::/128",    // unspecified
	), ipguard.UniqueLocalV6...)
	nat64Prefixes = ipguard.MustCIDRs(
		"64:ff9b::/96",   // well-known NAT64 (RFC 6052)
		"64:ff9b:1::/48", // local-use NAT64 (RFC 8215)
	)
}

// isBlockedIP reports whether ip is in an unconditionally-denied range.
// IPv4-mapped IPv6 addresses are unwrapped so a "::ffff:127.0.0.1" cannot
// smuggle a loopback target past the guard.
func isBlockedIP(ip net.IP) (bool, string) {
	if ip == nil {
		return true, "nil ip"
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true, "loopback/link-local/multicast"
	}
	if v4 := ip.To4(); v4 != nil {
		for _, n := range blockedV4 {
			if n.Contains(v4) {
				return true, "private/reserved v4 " + n.String()
			}
		}
		return false, ""
	}
	for _, n := range blockedV6 {
		if n.Contains(ip) {
			return true, "private/reserved v6 " + n.String()
		}
	}
	// NAT64-embedded IPv4 smuggling: inside a NAT64 prefix the low 32 bits ARE a
	// real IPv4, so 64:ff9b::a9fe:a9fe reaches 169.254.169.254 while To4()==nil.
	// Block the prefix wholesale (fail closed) and re-run the embedded v4 through
	// the v4 block check so the reason names the real target.
	// HONEST RESIDUAL: only well-known + local-use NAT64 prefixes are covered; a
	// network-specific RFC 6052 prefix is unknowable here without config. The
	// embedded check is scoped to NAT64 prefixes on purpose — running it on every
	// IPv6 would false-positive legit addresses whose low 32 bits happen to fall
	// in a reserved v4 range (e.g. any address ending ::1 -> 0.0.0.1 in 0/8).
	for _, n := range nat64Prefixes {
		if n.Contains(ip) {
			embedded := net.IP(ip.To16()[12:16])
			if blocked, why := isBlockedIP(embedded); blocked {
				return true, "nat64-embedded " + why
			}
			return true, "nat64 prefix " + n.String()
		}
	}
	return false, ""
}

// decisionLog builds the structured egress.DecisionLog for a request.
func decisionLog(req egress.Request, d egress.Decision, ruleSource string) egress.DecisionLog {
	return egress.DecisionLog{
		Request:    req,
		Decision:   d,
		RuleSource: ruleSource,
	}
}
