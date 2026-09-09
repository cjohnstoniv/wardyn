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
//     re-resolves the hostname (no TOCTOU). Under a corporate upstream the
//     PIN is relaxed (the corp proxy resolves and dials, so the target is sent
//     by name) but the GUARD is not: egressTarget resolves for the guard and
//     denies a name that answers into a blocked range. Its two stated residuals
//     are a name this proxy cannot resolve at all, which is forwarded to the
//     corp proxy's own egress controls, and — because the target is sent by
//     name — a name that answers differently to the two resolvers, which the
//     guard binds only at check time (see egressTarget, THREAT-MODEL.md §4.2).
//   - Fail closed on every error path.
package proxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
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

// canonHost is the ONE spelling every policy map is keyed on, at compile time
// and at lookup: lower-cased, trailing dot trimmed, and — when the string is an
// IP LITERAL — the canonical net.IP.String() form of it.
//
// The literal half is the part that was missing, and its absence was a deny
// bypass, not a cosmetic inconsistency. evalHost keyed on the raw request
// string while AllowsLiteralIP (the trusted-literal path, egress_target.go)
// keyed on ip.String(), so one policy answered two ways: a run that denies the
// literal 93.184.216.34 still allowed "::ffff:93.184.216.34" — which
// vetHostLift's literal fast path then parses back to that same address and
// dials — and under allow_all_egress (deny-list-only mode) that is the whole
// barrier gone. Literal-IP deny entries are a first-class shape here, not a
// misuse: ValidDomainEntry exempts IPv6 literals from the ":port" check, and
// literalIPDenialDetail composes an operator message about them.
//
// The same normalization on the ENTRY side (classifyDomain) is the other half:
// an operator who typed a non-canonical spelling into denied_domains had a dead
// entry that silently protected nothing, which is exactly what ValidDomainEntry
// exists to prevent. Both sides now land in the same space, so the two can no
// longer disagree.
//
// It does NOT widen an allow to a different destination: the deny lookups are
// canonicalized in the same call and still run first, and an allow that now
// matches a second spelling matches the SAME address the operator listed.
// Non-literal hosts are untouched (a hostname never parses as an IP), and a
// spelling net.ParseIP cannot read — "127.1", "0x7f000001", a zone-suffixed
// "fe80::1%eth0" — is left verbatim, which is the fail-closed answer here: it
// matches no allow entry, and the unconditional IP guard still binds the dial.
func canonHost(h string) string {
	h = strings.TrimSuffix(strings.ToLower(h), ".")
	if ip := net.ParseIP(h); ip != nil {
		return ip.String()
	}
	return h
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
	// canonHost, not the raw string: an entry spelled "::ffff:93.184.216.34" or
	// "2001:0db8:0000::1" must compile to the same key the request side derives
	// for that address, or the entry is dead. There is no wildcard IP form, so
	// only this branch can carry a literal.
	return canonHost(d), "", port
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
	host = canonHost(host)
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
	// later in the pipeline, in egressTarget) is unaffected, so allow-all reaches
	// PUBLIC hosts only — with TWO residuals, stated here because a reader who
	// takes "public only" as absolute would be wrong about them: under a
	// configured corporate upstream the corp proxy resolves and dials, so (1) a
	// name THIS proxy cannot resolve at all is forwarded to it unvetted
	// (egressTarget's upstream branch excuses guard.Unresolved, and only that; a
	// name that does resolve into blocked space is still denied), and (2) the
	// guard binds the name at CHECK time only — the corp proxy resolves again for
	// the dial, so a name that answers differently to the two resolvers
	// (short-TTL rebinding, or a split-horizon zone only the corp proxy can see)
	// is not bound at dial time. So under allow_all_egress plus an upstream,
	// "public only" is what this proxy can verify, not what it can prove — the
	// rest is the corp proxy's own egress controls. See egressTarget,
	// THREAT-MODEL.md §4.2 and docs/OPERATIONS.md's upstream section.
	// AllowedExactHost (credential injection) deliberately does NOT honor
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
	host = canonHost(host)
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
	// Its callers already pass ip.String(); canonHost is idempotent on that and
	// keeps all four policy lookups reading the same normalizer rather than
	// three of them plus one convention.
	host = canonHost(host)
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

// AuthoredPortFor reports whether the operator authored a PORT-QUALIFIED
// allowlist entry covering host:port — "vendor.example:8443", or the wildcard
// form "*.example.com:8443".
//
// It is the closest thing the compiled policy has to declared TRANSPORT INTENT
// (F110): a BARE entry matches any port, so it says nothing about which port
// the operator meant; a port-qualified one is the operator naming the port in
// writing. Credential injection over CLEARTEXT reads it that way — see
// Proxy.injectableTransport, which asks this only AFTER its unconditional
// clamp on port 443, so an authored `host:443` can never re-admit a cleartext
// credential to the TLS port. Deny still beats allow, on the same two lookups
// AllowsLiteralIP uses.
func (p *Policy) AuthoredPortFor(host string, port int) bool {
	if p == nil {
		return false
	}
	host = canonHost(host)
	if _, ok := p.deniedExact[host]; ok {
		return false
	}
	if _, ok := p.deniedExactPort[hostPortKey(host, port)]; ok {
		return false
	}
	if matchWildPort(host, port, p.deniedWildPort) {
		return false
	}
	if _, ok := p.allowedExactPort[hostPortKey(host, port)]; ok {
		return true
	}
	return matchWildPort(host, port, p.allowedWildPort)
}

// egressHeaderDetail carries the CAUSE behind an address-range refusal, beside
// the static rule_source egressHeaderReason already carries.
//
// It exists because "builtin:private-ip" names the RULE and never the reason it
// fired, and those are two different questions with two different fixes: a
// literal IP that no allowlist entry names is fixed in the policy (or by the
// egress redirect that would add it); a HOSTNAME that resolves into private
// space is fixed in site config, by declaring it under internal_hosts. An
// operator who cannot tell those apart reads a correct private-endpoint
// configuration as broken — the exact misdirection the private-endpoint work
// exists to remove. The value is composed from a canonical net.IP string and
// fixed sentences; it never echoes the requested hostname (X-Wardyn-Host
// already carries that).
const egressHeaderDetail = "X-Wardyn-Egress-Detail"

// literalIPDenialDetail is egressHeaderDetail's value for a builtin:private-ip
// refusal of host: which of the three causes fired, and the one place to fix
// it. Returns "" when host is neither a literal IP nor a hostname (i.e. there
// is nothing specific to say), so callers can skip the header.
func literalIPDenialDetail(host string, port int, pol *Policy) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return ""
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return "this host resolves into a private/reserved address range, which the built-in guard denies regardless of policy; " +
			"declare it in site config under internal_hosts (host_suffix plus the cidrs it may resolve into) to lift the guard for it"
	}
	if pol != nil {
		if _, denied := pol.deniedExact[ip.String()]; denied {
			return "literal IP " + ip.String() + " is on denied_domains, and a deny always beats an allow"
		}
		if _, denied := pol.deniedExactPort[hostPortKey(ip.String(), port)]; denied {
			return "literal IP " + ip.String() + " is on denied_domains for this port, and a deny always beats an allow"
		}
	}
	return "literal IP " + ip.String() + " is in a private/reserved range, so only an EXACT allowed_domains entry for that address can reach it — " +
		"it is not listed; an egress_redirects \"to\" pointing at this address adds that entry automatically for the runs it covers"
}

// resolveFailedDetail is egressHeaderDetail's value for a builtin:resolve-failed
// refusal: the proxy never learned an address for the name, so nothing was
// vetted and nothing about the policy or the address-range guard explains the
// deny. It is a FIXED sentence like the ones above and never echoes the host —
// X-Wardyn-Host already carries that.
//
// It exists because the deny used to arrive labelled builtin:private-ip with
// literalIPDenialDetail's "declare it under internal_hosts" advice attached
// (F055): advice that cannot fix a resolver outage, pointed at loosening an
// SSRF control, for a fault that is neither.
const resolveFailedDetail = "this host did not resolve (DNS failure, no such name, or no address records), so no address " +
	"could be vetted; this is a name-resolution fault, not the private-address guard — check the sandbox's resolver, " +
	"not the allowlist"

// writeEgressDeny writes the 403 both forward paths (handlePlain,
// handleConnect) give a DENIED request: the static refusal headers, plus — for
// the one refusal an operator reliably misreads — the cause and where to fix
// it. Every other refusal reason already says all there is to say, so its body
// and headers stay byte-identical.
//
// It lives here rather than in proxy.go beside its two callers so it sits with
// the rule it explains (and so proxy.go stays under the 1000-line split gate).
func (p *Proxy) writeEgressDeny(w http.ResponseWriter, host string, port int, log *egress.DecisionLog) {
	body := "egress denied by policy"
	switch decisionReason(log) {
	case "builtin:private-ip":
		if detail := literalIPDenialDetail(host, port, p.policy); detail != "" {
			w.Header().Set(egressHeaderDetail, detail)
			body = "egress denied: " + detail
		}
	case "builtin:resolve-failed":
		w.Header().Set(egressHeaderDetail, resolveFailedDetail)
		body = "egress denied: " + resolveFailedDetail
	}
	setEgressRefusalHeadersWithReason(w, egressRefusalDenied, host, decisionReason(log))
	http.Error(w, body, http.StatusForbidden)
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
	// Unresolved distinguishes a denial that means "this proxy could not learn
	// the addresses at all" (local DNS failed, or answered with nothing) from
	// one that means "an address is blocked". egressTarget's corp-upstream
	// branch reads it to EXCUSE the case: under an operator upstream the sandbox
	// host frequently CANNOT resolve external names, which is the whole reason
	// that branch exists, so a resolve failure there must not become a denial —
	// while a name that DOES resolve into blocked space must be, which it was
	// not before. egressTarget's direct-dial branch reads it to ATTRIBUTE the
	// case (errHostUnresolved -> builtin:resolve-failed): there it still denies,
	// but it is a DNS fault and not the address-range guard, and the two have
	// opposite fixes. Every other caller treats Denied as Denied.
	Unresolved bool
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
		return IPGuardResult{Denied: true, Unresolved: true, Reason: fmt.Sprintf("resolve failed: %v", err)}
	}
	if len(ips) == 0 {
		return IPGuardResult{Denied: true, Unresolved: true, Reason: "no addresses"}
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
// unconditionally, by construction (never offered to lift). blockNAT64 and
// blockV4Compat are the two EMBEDDED-v4 shapes: an IPv6 literal whose low 32
// bits are a blocked IPv4 that To4() cannot see.
type blockKind uint8

const (
	blockNone          blockKind = iota // not blocked
	blockLocal                          // loopback/link-local/multicast/unspecified/nil — never liftable
	blockPrivate                        // RFC1918/ULA/CGNAT — the ONLY liftable kind (ipguard.Liftable)
	blockReservedOther                  // other ipguard.ReservedV4/ReservedV6 entries — never liftable
	blockNAT64                          // NAT64-embedded smuggling — never liftable
	blockV4Compat                       // IPv4-compatible ::/96-embedded smuggling — never liftable
)

// v4CompatiblePrefix is the DEPRECATED IPv4-compatible IPv6 range (RFC 4291
// §2.5.5.1): "::127.0.0.1" and "::169.254.169.254" carry a real IPv4 in their
// low 32 bits, exactly as a NAT64 prefix does.
//
// It is NOT in ipguard.ReservedV6 (which denies wholesale) on purpose: the
// prefix also contains :: and ::1, which the stdlib predicates already name
// precisely, and denying all of ::/96 would refuse an address whose embedded
// v4 is public. Only the embedded address decides — see isBlockedIP.
var v4CompatiblePrefix = netip.MustParsePrefix("::/96")

// v4CompatibleEmbeddedV4 returns the IPv4 embedded in the low 32 bits of an
// IPv4-COMPATIBLE ::/96 address, and (nil, false) for anything else —
// including an IPv4-mapped ::ffff:/96 address, which Unmap turns back into the
// IPv4 the canonical path already judges.
//
// TRUST BOUNDARY: deny-only, like nonCanonicalLiteralIP. Its one consumer
// (isBlockedIP) uses it to DENY; the extracted address is never offered to
// trustsExactLiteralIP, so a spelling the operator did not type inherits no
// allowed_domains grant.
func v4CompatibleEmbeddedV4(ip net.IP) (net.IP, bool) {
	addr, ok := netip.AddrFromSlice(ip)
	if !ok {
		return nil, false
	}
	if addr = addr.Unmap(); !addr.Is6() || !v4CompatiblePrefix.Contains(addr) {
		return nil, false
	}
	b := addr.As16()
	return net.IP(b[12:16]), true
}

// isBlockedIP reports whether ip is in an unconditionally-denied range:
// loopback, link-local (incl. the 169.254.169.254 metadata address), multicast,
// the unspecified address, RFC1918/ULA private space and the reserved ranges in
// internal/ipguard. Denied regardless of policy: the proxy denies
// loopback/link-local ALWAYS — no operator setting lifts those, and
// SiteConfig.InternalHosts (the one override that exists) cannot reach them,
// because its CIDRs must lie inside ipguard.Liftable and that set never
// intersects loopback, link-local or the metadata address. This is exactly what
// the net.IP
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
	//
	// 6to4 (2002::/16) is the OTHER embedded-v4 shape — 2002:7f00:0001::1 carries
	// 127.0.0.1 — and it is NOT handled here: its IPv4 sits at bits 16..48, which
	// this extraction cannot read. It is denied a step earlier instead, wholesale
	// via ipguard.ReservedV6 (deprecated and unroutable per RFC 7526), so it
	// reaches this branch as blockReservedOther and never as blockNAT64.
	if embedded, ok := ipguard.NAT64EmbeddedV4(ip); ok {
		if kind, why := isBlockedIP(embedded); kind != blockNone {
			return blockNAT64, "nat64-embedded " + why
		}
		return blockNAT64, "nat64 prefix (RFC 6052/8215)"
	}
	// The IPv4-COMPATIBLE ::/96 form is the THIRD embedded-v4 shape, and the one
	// the predicates above cannot see: "::127.0.0.1" and "::169.254.169.254"
	// carry a real IPv4 in their low 32 bits, and net.ParseIP PARSES them — so
	// unlike the inet_aton spellings there is nothing for literal_ip_guard.go's
	// gap-filler to fill. They arrive here on the CANONICAL path with To4() ==
	// nil (To4 unwraps only ::ffff:/96), IsLoopback/IsLinkLocalUnicast answer
	// false, and PrivateReserved has no ::/96 entry — so the address walked past
	// step 0 AND past vetHostLift's literal fast path, leaving nothing but the
	// default-deny allowlist between that spelling and a dial. Re-run the
	// embedded v4 the same way the NAT64 arm does, so the denial names the real
	// target and both guards agree with what dials.
	//
	// Only the embedded address decides (the prefix is not denied wholesale, and
	// :: / ::1 are already named above), so this can only ADD denials that the
	// canonical spelling of the same address already gets.
	if embedded, ok := v4CompatibleEmbeddedV4(ip); ok {
		if kind, why := isBlockedIP(embedded); kind != blockNone {
			return blockV4Compat, "ipv4-compatible-embedded " + why
		}
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
