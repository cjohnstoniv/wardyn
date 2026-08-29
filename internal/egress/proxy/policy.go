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
	// toolRules is tool name -> effect, compiled from RunPolicySpec.ToolRules.
	// Nil/empty means "no rules", which is today's behaviour: every gated call
	// raises an approval. The "*" key is the default for unmatched tools.
	toolRules map[string]types.ToolEffect
	// allowAll switches evalHost from default-deny (allowlist only) to "allow
	// all (deny-list only)": a non-denied host resolves to hostAllow even when
	// it is not in allowedExact/allowedWild. Deny still beats allow, the
	// unconditional VetHost/isBlockedIP private-IP guard is unaffected, and
	// AllowedExactHost (credential injection) is unchanged — injection still
	// requires an explicit exact allowlist entry even under allow-all.
	allowAll bool
	// gitPushAnyBranch mirrors RunPolicySpec.GitPushAnyBranch: this run's
	// brokered pushes skip branch-namespace confinement (handleGitBroker). The
	// per-run counterpart of the deployment-wide BranchNSEnforced() switch.
	gitPushAnyBranch bool
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
		gitPushAnyBranch: spec.GitPushAnyBranch,
	}
	// Compiled into a map rather than scanned: validatePolicySpec already refuses
	// duplicates, so the map cannot lose a rule, and an exact-match lookup is the
	// whole matching semantics.
	if len(spec.ToolRules) > 0 {
		p.toolRules = make(map[string]types.ToolEffect, len(spec.ToolRules))
		for _, r := range spec.ToolRules {
			p.toolRules[r.Tool] = r.Effect
		}
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

// GitPushAnyBranch reports whether this run's policy opts its brokered pushes
// out of branch-namespace confinement. Nil-safe like ToolEffectFor: a proxy
// built without a compiled policy keeps confinement ON.
func (p *Policy) GitPushAnyBranch() bool { return p != nil && p.gitPushAnyBranch }

// FirstUseMode reports how unknown domains are handled (always_deny /
// deny_with_review / wait_for_review), normalized (never empty).
func (p *Policy) FirstUseMode() types.FirstUseMode { return p.firstUse }

// ToolEffectFor reports what a run's policy says about one tool call, and
// whether a rule matched at all.
//
// Match order: the exact tool name, then the "*" default. There is no pattern
// matching — see ToolRule's doc for why a glob over tool names is the wrong
// shape here.
//
// A run with NO rules returns ok=false, and the caller must fall back to today's
// behaviour (raise an approval). That fallback is what makes this field additive:
// a policy authored before it behaves exactly as it did.
func (p *Policy) ToolEffectFor(tool string) (types.ToolEffect, bool) {
	if p == nil || len(p.toolRules) == 0 {
		return "", false
	}
	if e, ok := p.toolRules[tool]; ok {
		return e, true
	}
	if e, ok := p.toolRules["*"]; ok {
		return e, true
	}
	return "", false
}

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

// ValidDomainEntry reports whether d is an allowlist/denylist entry the matcher
// above can ever match, and is the ONE shape check every operator-supplied
// policy ingest point runs (validatePolicySpec, internal/api). classifyDomain
// accepts anything — a mid-label pattern like "oidc.*.amazonaws.com" compiles
// to an exact hostname no real request can equal, so it silently protects
// nothing. Operator input fails closed instead: reject the dead entry at write
// time rather than ship a policy the operator believes is guarding them.
//
// Valid: a bare exact host ("api.anthropic.com"), a leading-"*." wildcard
// ("*.example.com"), and either with a valid ":port" qualifier.
func ValidDomainEntry(d string) error {
	exact, wild, _ := classifyDomain(d)
	bad := func(why string) error {
		return fmt.Errorf("domain %q never matches any request (%s); supported forms: "+
			`"example.com", "*.example.com", "example.com:443", "*.example.com:443"`, d, why)
	}
	switch {
	case exact == "" && wild == "":
		return bad("empty")
	case strings.Contains(exact, "*"), strings.Contains(wild, "*"):
		return bad(`a "*" is only supported as a leading "*."`)
	case strings.ContainsAny(exact, "/ \t"), strings.ContainsAny(wild, "/ \t"):
		return bad("must be a bare host, not a URL")
	// classifyDomain leaves a malformed ":port" attached to the host (so it
	// cannot silently widen to any-port) — which makes it a dead entry.
	// An IPv6 literal legitimately contains ':', so exempt it.
	case strings.Contains(exact, ":") && net.ParseIP(exact) == nil:
		return bad(`the ":port" qualifier must be a number in 1..65535`)
	// Same check on the wildcard branch, which is otherwise unguarded: a valid
	// ":port" is stripped into the port by classifyDomain, and there is no IPv6
	// wildcard form, so ANY residual ':' here is a malformed qualifier. Without
	// this, "*.example.com:0" compiles to the suffix ".example.com:0" — which no
	// request host can end with, since evalHost is handed host and port
	// separately. That is precisely the "policy that lies" this function exists
	// to reject, and it was slipping through the branch the exact case guards.
	case strings.Contains(wild, ":"):
		return bad(`the ":port" qualifier must be a number in 1..65535`)
	}
	return nil
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

// AllowsLiteralIP reports whether host (a literal IP address, already
// lowercased with any trailing dot trimmed) is explicitly present in this
// policy's EXACT allowlist for port — either a bare entry (matches any port)
// or a port-qualified one. Only an exact, operator-authored entry counts
// (never a wildcard): a literal IP the operator typed into AllowedDomains
// directly (e.g. an egress-redirect "To" target or corp registry that lives
// on RFC1918 space, per SiteConfig.EgressRedirects) carries none of the
// DNS-rebinding risk the unconditional private-IP guard exists to catch —
// there is no hostname to rebind — so evaluate() treats it as trusted
// instead of hard-denying it (W13-S1-3). Deny still beats allow.
func (p *Policy) AllowsLiteralIP(host string, port int) bool {
	if _, ok := p.deniedExact[host]; ok {
		return false
	}
	if _, ok := p.deniedExactPort[hostPortKey(host, port)]; ok {
		return false
	}
	if _, ok := p.allowedExact[host]; ok {
		return true
	}
	_, ok := p.allowedExactPort[hostPortKey(host, port)]
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
	// Lifted is true when an address that isBlockedIP would otherwise deny was
	// admitted only because the caller's lift predicate (vetHostLift) accepted
	// it — i.e. an operator-declared internal host (SiteConfig.InternalHosts).
	// Never true for VetHost (which always calls vetHostLift with a nil lift).
	Lifted bool
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
//
// VetHost is vetHostLift with no lift predicate — nothing in blockPrivate is
// ever admitted. Kept as the exported, unconditional guard so every existing
// caller/test keeps today's behavior; see vetHostLift for the internal-host
// exception (Proxy.vetHost).
func VetHost(host string, res resolver) IPGuardResult {
	return vetHostLift(host, res, nil)
}

// vetHostLift is VetHost with one addition: when an address is blocked ONLY
// because it is private/reserved (blockKind == blockPrivate — RFC1918/ULA/CGNAT,
// never loopback/link-local/metadata/unspecified/multicast/NAT64, which stay
// unconditionally denied), lift optionally admits it. lift == nil behaves
// exactly like VetHost. Reason strings and the fail-closed shape (empty host /
// resolve failure / no addresses / any blocked answer denies the whole host)
// are unchanged from VetHost.
func vetHostLift(host string, res resolver, lift func(net.IP) bool) IPGuardResult {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return IPGuardResult{Denied: true, Reason: "empty host"}
	}
	admit := func(ip net.IP) (bool, bool, string) { // ok, lifted, reason
		kind, why := isBlockedIP(ip)
		if kind == blockNone {
			return true, false, ""
		}
		if kind == blockPrivate && lift != nil && lift(ip) {
			return true, true, ""
		}
		return false, false, why
	}
	// Literal IP fast path.
	if ip := net.ParseIP(host); ip != nil {
		ok, lifted, why := admit(ip)
		if !ok {
			return IPGuardResult{Denied: true, Reason: why}
		}
		return IPGuardResult{IP: ip, Lifted: lifted}
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
	// If ANY resolved address is blocked (and not lifted), deny the whole host: a
	// mix of public and private answers is the classic DNS-rebinding attack
	// shape. Fail closed.
	anyLifted := false
	for _, ip := range ips {
		ok, lifted, why := admit(ip)
		if !ok {
			return IPGuardResult{Denied: true, Reason: fmt.Sprintf("blocked address %s: %s", ip, why)}
		}
		anyLifted = anyLifted || lifted
	}
	return IPGuardResult{IP: ips[0], Lifted: anyLifted}
}

// blockKind classifies why isBlockedIP denies an address. Only blockPrivate is
// ever eligible for vetHostLift's lift predicate — the other kinds are denied
// unconditionally, by construction (never offered to lift).
type blockKind uint8

const (
	blockNone          blockKind = iota // not blocked
	blockLocal                          // loopback/link-local/multicast/unspecified/nil — never liftable
	blockPrivate                        // RFC1918/ULA/CGNAT — the ONLY liftable kind (ipguard.Liftable)
	blockReservedOther                  // other internal/ipguard.ReservedV4 entries — never liftable
	blockNAT64                          // NAT64-embedded smuggling — never liftable
)

// isBlockedIP reports whether ip is in an unconditionally-denied range:
// loopback, link-local (incl. the 169.254.169.254 metadata address), multicast,
// the unspecified address, RFC1918/ULA private space and the reserved ranges in
// internal/ipguard. Denied regardless of policy — unlike the composer transport
// (which spares loopback under its operator allowPrivate escape hatch), the
// proxy denies loopback/link-local ALWAYS, which is exactly what the net.IP
// predicates below give (they cover 127.0.0.0/8, ::1, 169.254.0.0/16, fe80::/10
// and 0.0.0.0 / :: precisely, so no proxy-local CIDR table is needed on top).
// IPv4-mapped IPv6 addresses are unwrapped so a "::ffff:127.0.0.1" cannot
// smuggle a loopback target past the guard.
//
// The returned blockKind is finer than a bool ONLY so vetHostLift can tell
// apart the one liftable case (blockPrivate — RFC1918/ULA/CGNAT,
// ipguard.Liftable) from every other denial, which stays unconditional. Reason
// strings are unchanged from before blockKind existed.
func isBlockedIP(ip net.IP) (blockKind, string) {
	if ip == nil {
		return blockLocal, "nil ip"
	}
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return blockLocal, "loopback/link-local/multicast"
	}
	if blocked, why := ipguard.PrivateReserved(ip); blocked {
		if ipguard.InLiftable(ip) {
			return blockPrivate, "private/reserved " + why
		}
		return blockReservedOther, "private/reserved " + why
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
	if embedded, ok := ipguard.NAT64EmbeddedV4(ip); ok {
		if kind, why := isBlockedIP(embedded); kind != blockNone {
			return blockNAT64, "nat64-embedded " + why
		}
		return blockNAT64, "nat64 prefix (RFC 6052/8215)"
	}
	return blockNone, ""
}

// decisionLog builds the structured egress.DecisionLog for a request.
func decisionLog(req egress.Request, d egress.Decision, ruleSource string) egress.DecisionLog {
	return egress.DecisionLog{
		Request:    req,
		Decision:   d,
		RuleSource: ruleSource,
	}
}
