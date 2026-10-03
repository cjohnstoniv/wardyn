// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package proxy implements the L2 per-workspace egress sidecar (wardyn-proxy):
// an HTTP forward proxy that enforces the internal/egress decision model
// (default-deny domain allowlist, method rules, first-use approval),
// streams decision logs, and injects credentials proxy-side.
//
// Security invariants (mirror ARCHITECTURE.md and internal/egress):
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
	"github.com/cjohnstoniv/wardyn/internal/egress/domainmatch"
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
	// allowedExactAnyPort is the host-keyed INDEX of allowedExactPort, answering
	// the HOST question AllowedExactHost asks (credential injection) — evalHost
	// never consults it, so a port-qualified entry still grants egress on that
	// port only. Keeps the PORTS (rather than stripping) so the allow and deny
	// sides stay symmetric: a stripped shadow would let "allow m.corp:443 + deny
	// m.corp:443" build an injector the port-less code failed closed on.
	allowedExactAnyPort map[string]map[int]struct{}
	allowedWildPort     []domainmatch.WildPort
	deniedExactPort     map[string]struct{}
	deniedWildPort      []domainmatch.WildPort
	allowedMeth         map[string]struct{} // empty == all methods allowed
	firstUse            types.FirstUseMode
	// toolRules is tool name -> effect, compiled from RunPolicySpec.ToolRules.
	// Nil/empty means "no rules": every gated call raises an approval. "*" is
	// the default for unmatched tools.
	toolRules map[string]types.ToolEffect
	// allowAll switches evalHost from default-deny to "allow all (deny-list
	// only)": a non-denied host resolves to hostAllow even outside the
	// allowlist. Deny still beats allow, the unconditional private-IP guard is
	// unaffected, and AllowedExactHost (credential injection) still requires an
	// explicit exact allowlist entry.
	allowAll bool
	// gitPushAnyBranch mirrors RunPolicySpec.GitPushAnyBranch: this run's
	// brokered pushes skip branch-namespace confinement (handleGitBroker).
	gitPushAnyBranch bool
	// pushRules is RunPolicySpec.PushRules compiled for per-request matching, or
	// nil when the run carries no content rule at all. Compiled here rather
	// than per request since a push may be matched against every entry of a
	// list the control-plane body cap alone bounds (push_rules.go).
	pushRules *pushRuleSet
}

// CompilePolicy builds a Policy from a RunPolicySpec. Domains are normalized
// to lowercase; trailing dots are stripped. Methods are uppercased.
func CompilePolicy(spec types.RunPolicySpec) *Policy {
	p := &Policy{
		allowedExact:        make(map[string]struct{}),
		deniedExact:         make(map[string]struct{}),
		allowedExactPort:    make(map[string]struct{}),
		allowedExactAnyPort: make(map[string]map[int]struct{}),
		deniedExactPort:     make(map[string]struct{}),
		allowedMeth:         make(map[string]struct{}),
		firstUse:            spec.FirstUseApproval.Normalize(),
		allowAll:            spec.AllowAllEgress,
		gitPushAnyBranch:    spec.GitPushAnyBranch,
		pushRules:           compilePushRules(spec.PushRules),
	}
	// Compiled into a map rather than scanned: validatePolicySpec already refuses
	// duplicates, so an exact-match lookup is the whole matching semantics.
	if len(spec.ToolRules) > 0 {
		p.toolRules = make(map[string]types.ToolEffect, len(spec.ToolRules))
		for _, r := range spec.ToolRules {
			p.toolRules[r.Tool] = r.Effect
		}
	}
	for _, d := range spec.AllowedDomains {
		exact, wild, port := domainmatch.Classify(d)
		switch {
		case wild != "" && port > 0:
			p.allowedWildPort = append(p.allowedWildPort, domainmatch.WildPort{Suffix: wild, Port: port})
		case wild != "":
			p.allowedWild = append(p.allowedWild, wild)
		case exact != "" && port > 0:
			p.allowedExactPort[hostPortKey(exact, port)] = struct{}{}
			if p.allowedExactAnyPort[exact] == nil {
				p.allowedExactAnyPort[exact] = make(map[int]struct{})
			}
			p.allowedExactAnyPort[exact][port] = struct{}{}
		case exact != "":
			p.allowedExact[exact] = struct{}{}
		}
	}
	for _, d := range spec.DeniedDomains {
		exact, wild, port := domainmatch.Classify(d)
		switch {
		case wild != "" && port > 0:
			p.deniedWildPort = append(p.deniedWildPort, domainmatch.WildPort{Suffix: wild, Port: port})
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
// out of branch-namespace confinement. Nil-safe like ToolEffectFor.
func (p *Policy) GitPushAnyBranch() bool { return p != nil && p.gitPushAnyBranch }

// PushRulesSet reports whether this run's policy carries git push CONTENT
// rules. Nil-safe like GitPushAnyBranch.
func (p *Policy) PushRulesSet() bool { return p.contentRules() != nil }

// contentRules returns the compiled push content rules, or nil when the run
// has none. Nil-safe on the same ground PushRulesSet is.
func (p *Policy) contentRules() *pushRuleSet {
	if p == nil {
		return nil
	}
	return p.pushRules
}

// FirstUseMode reports how unknown domains are handled (always_deny /
// deny_with_review / wait_for_review), normalized (never empty).
func (p *Policy) FirstUseMode() types.FirstUseMode { return p.firstUse }

// ToolEffectFor reports what a run's policy says about one tool call, and
// whether a rule matched at all.
//
// Match order: the exact tool name, then the "*" default. No pattern matching
// — see ToolRule's doc for why a glob over tool names is the wrong shape.
//
// A run with NO rules returns ok=false, and the caller must fall back to
// today's behaviour (raise an approval) — what makes this field additive.
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

// hostPortKey is the map key for a port-qualified exact entry. It must be built
// identically at compile time and at lookup so "host:443" collides correctly.
func hostPortKey(host string, port int) string {
	return host + ":" + strconv.Itoa(port)
}

// ValidDomainEntry reports whether d is an allowlist/denylist entry the matcher
// above can ever match — the ONE shape check every operator-supplied policy
// ingest point runs (validatePolicySpec). domainmatch.Classify accepts anything (a
// mid-label pattern like "oidc.*.amazonaws.com" compiles to a hostname no real
// request can equal), so operator input fails closed instead of shipping a
// policy the operator believes is guarding them.
//
// Valid: a bare exact host ("api.anthropic.com"), a leading-"*." wildcard
// ("*.example.com"), and either with a valid ":port" qualifier.
func ValidDomainEntry(d string) error {
	exact, wild, _ := domainmatch.Classify(d)
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
	// domainmatch.Classify leaves a malformed ":port" attached (so it can't silently
	// widen to any-port), which makes it a dead entry. IPv6 legitimately
	// contains ':', so exempt it.
	case strings.Contains(exact, ":") && net.ParseIP(exact) == nil:
		return bad(`the ":port" qualifier must be a number in 1..65535`)
	// Same check on the wildcard branch: a valid ":port" is stripped by
	// domainmatch.Classify and there's no IPv6 wildcard form, so any residual ':'
	// here is malformed. Without this, "*.example.com:0" compiles to a suffix
	// no request host can end with.
	case strings.Contains(wild, ":"):
		return bad(`the ":port" qualifier must be a number in 1..65535`)
	// Charset. See domain_charset.go.
	case deadCharsetEntry(exact, wild):
		return bad(charsetWhy)
	}
	return nil
}

// evalHost returns the policy-only verdict for a host+port. Deny always wins.
// A bare allow/deny entry matches any port; a port-qualified entry ("host:443")
// matches only that host+port.
func (p *Policy) evalHost(host string, port int) hostDecision {
	host = domainmatch.CanonHost(host)
	key := hostPortKey(host, port)
	// Deny beats allow, unconditionally.
	if _, ok := p.deniedExact[host]; ok {
		return hostDeny
	}
	if _, ok := p.deniedExactPort[key]; ok {
		return hostDeny
	}
	if domainmatch.MatchWild(host, p.deniedWild) || domainmatch.MatchWildPort(host, port, p.deniedWildPort) {
		return hostDeny
	}
	if _, ok := p.allowedExact[host]; ok {
		return hostAllow
	}
	if _, ok := p.allowedExactPort[key]; ok {
		return hostAllow
	}
	if domainmatch.MatchWild(host, p.allowedWild) || domainmatch.MatchWildPort(host, port, p.allowedWildPort) {
		return hostAllow
	}
	// Allow-all (deny-list only) mode: any host surviving the deny checks
	// above is allowed. Runs AFTER the deny checks so denied_domains still
	// wins; the unconditional private-IP guard (applied later, in
	// egressTarget) is unaffected, so allow-all reaches PUBLIC hosts only —
	// with TWO residuals under a configured corporate upstream: (1) a name
	// THIS proxy cannot resolve at all is forwarded to it unvetted
	// (egressTarget excuses guard.Unresolved only; a name that DOES resolve
	// into blocked space is still denied), and (2) the guard binds the name
	// at CHECK time only — the corp proxy resolves again for the dial, so a
	// name answering differently to the two resolvers (rebinding, split-horizon)
	// is not bound at dial time. So "public only" here is what this proxy can
	// verify, not prove — the rest is the corp proxy's own controls. See
	// egressTarget, THREAT-MODEL.md §4.2. AllowedExactHost (credential
	// injection) deliberately does NOT honor allowAll.
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

// AllowsLiteralIP reports whether host (a literal IP address, already
// lowercased with any trailing dot trimmed) is explicitly present in this
// policy's EXACT allowlist for port — either a bare entry (matches any port)
// or a port-qualified one. Only an exact, operator-authored entry counts
// (never a wildcard): a literal IP the operator typed directly carries none
// of the DNS-rebinding risk the unconditional private-IP guard exists to
// catch (there's no hostname to rebind), so evaluate() trusts it instead of
// hard-denying. Deny still beats allow.
func (p *Policy) AllowsLiteralIP(host string, port int) bool {
	// Callers already pass ip.String(); domainmatch.CanonHost is idempotent on that.
	host = domainmatch.CanonHost(host)
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
// The closest thing the compiled policy has to declared TRANSPORT INTENT: a
// BARE entry says nothing about which port was meant; a port-qualified one is
// the operator naming it in writing. Credential injection over CLEARTEXT
// reads it that way (Proxy.injectableTransport), asked only AFTER its
// unconditional clamp on port 443, so an authored `host:443` can never
// re-admit a cleartext credential to the TLS port. Deny still beats allow, on
// the same two lookups AllowsLiteralIP uses.
func (p *Policy) AuthoredPortFor(host string, port int) bool {
	if p == nil {
		return false
	}
	host = domainmatch.CanonHost(host)
	if _, ok := p.deniedExact[host]; ok {
		return false
	}
	if _, ok := p.deniedExactPort[hostPortKey(host, port)]; ok {
		return false
	}
	if domainmatch.MatchWildPort(host, port, p.deniedWildPort) {
		return false
	}
	if _, ok := p.allowedExactPort[hostPortKey(host, port)]; ok {
		return true
	}
	return domainmatch.MatchWildPort(host, port, p.allowedWildPort)
}

// egressHeaderDetail carries the CAUSE behind an address-range refusal, beside
// the static rule_source egressHeaderReason already carries.
//
// "builtin:private-ip" names the RULE but never the reason it fired, and those
// have different fixes: a literal IP no allowlist names is fixed in the
// policy; a HOSTNAME resolving into private space is fixed in site config
// under internal_hosts. Composed from a canonical net.IP string and fixed
// sentences; it never echoes the requested hostname (X-Wardyn-Host already
// carries that).
const egressHeaderDetail = "X-Wardyn-Egress-Detail"

// egressHeaderRetry / egressRetryNever tell the sandbox a refusal is TERMINAL
// for this run, so a client with its own retry loop can stop instead of
// spending ten attempts on an answer that cannot change.
//
// Set on the builtin:private-ip arm below and on NOTHING else: the
// internal_hosts lift is compiled into this sidecar's config at dispatch and
// read once at startup, so the guard cannot change its mind mid-run. A
// builtin:resolve-failed may clear on the next attempt, and an
// approval-pending refusal is waiting for a human — "never" on either would
// turn a transient fault into a dead run.
const (
	egressHeaderRetry = "X-Wardyn-Egress-Retry"
	egressRetryNever  = "never"
)

// DRAFT (M2 canon pending) — the FIVE sentences a builtin:private-ip 403 can tell
// a HOSTNAME's operator, each its own constant so the owner's canon sitting is a
// one-line diff. Four compose the LIFTABLE refusal (cause, remedy, the console's
// second copy of the cidrs hint, lifetime); the fifth replaces the remedy and the
// lifetime clause on the never-liftable arm, where neither is true.
// literalIPDenialDetail joins the liftable four in exactly one place (its hostname
// arm below) and nothing else concatenates them.
//
// The canon keys these carry on the M2 sheet are named beside each one. They are
// server-composed, not console copy: this text reaches a human as an
// X-Wardyn-Egress-Detail header and a 403 body, and docs/OPERATIONS.md quotes
// the hint verbatim.
const (
	// egressPrivateRangeCause is today's sentence, unchanged.
	egressPrivateRangeCause = "this host resolves into a private/reserved address range, " +
		"which the built-in guard denies regardless of policy"
	// egressInternalHostsRemedy: leave cidrs empty unless the ranges are the
	// SANDBOX's, not an operator's own machine's corporate-resolver view — a
	// list drawn from the wrong side excludes the address the guard actually
	// refuses. Empty cidrs is the full liftable set, still suffix-scoped.
	egressInternalHostsRemedy = "declare it in site config under internal_hosts " +
		"(host_suffix; leave cidrs empty unless you know the ranges the SANDBOX resolves into) " +
		"to lift the guard for it"
	// siteInternalHostsCIDRHint restates the remedy's parenthetical a SECOND
	// time, deliberately: the two land on different readers, and the owner's
	// canon may keep either alone.
	siteInternalHostsCIDRHint = "Leave `cidrs` empty unless you know the addresses the sandbox resolves. " +
		"What your own machine sees for a private endpoint is usually not what the cluster sees."
	// egressDenialSuffix: the internal_hosts lift is compiled into this
	// sidecar's config at dispatch and read once at startup, so a site-config
	// change cannot reach the current run.
	egressDenialSuffix = "Site config is read at run start, so change it and start a new run — " +
		"this one will keep being refused."
	// egressNeverLiftableRemedy is the ANTI-remedy: only blockPrivate
	// (RFC1918/ULA/CGNAT) is liftable (vetHostLift offers the lift predicate to
	// that kind alone); every other class — loopback, link-local/metadata,
	// NAT64/::/96-embedded, and other reserved ranges — got the same four
	// sentences before this existed, instructing the operator into a
	// kill-and-redispatch cycle that could never succeed. NO site-config
	// remedy and NO lifetime clause on purpose: there's nothing to change.
	egressNeverLiftableRemedy = "which the proxy denies unconditionally — loopback, link-local " +
		"(including the 169.254.169.254 metadata address), multicast, NAT64- and " +
		"IPv4-compatible-embedded and the other reserved ranges are never reachable from a sandbox, " +
		"and no internal_hosts entry lifts them. There is nothing to change in site config."
)

// neverLiftableClass names the guard class that refused, for the one-sentence
// detail above. blockPrivate and blockNone are absent deliberately: blockPrivate
// is the LIFTABLE kind (it gets the four-sentence remedy instead) and blockNone
// never refuses, so a lookup miss falls back to the liftable wording — today's
// behaviour, the safe direction for an unknown caller.
var neverLiftableClass = map[blockKind]string{
	blockLocal:         "a loopback, link-local/metadata, multicast or unspecified address",
	blockReservedOther: "a reserved address",
	blockNAT64:         "a NAT64-embedded address",
	blockV4Compat:      "an IPv4-compatible-embedded address",
}

// neverLiftableDetail composes the never-liftable sentence for `subject` ("this
// host's address", or "literal IP 10.0.0.1"). Returns "" when kind IS liftable,
// so the single caller can fall through to the remedy text.
func neverLiftableDetail(subject string, kind blockKind) string {
	class, ok := neverLiftableClass[kind]
	if !ok {
		return ""
	}
	return subject + " is " + class + ", " + egressNeverLiftableRemedy
}

// literalIPDenialDetail is egressHeaderDetail's value for a builtin:private-ip
// refusal of host: which of the three causes fired, and the one place to fix
// it. Returns "" when host is neither a literal IP nor a hostname.
//
// kind is the GUARD CLASS that refused, carried out of the vet rather than
// re-derived (re-deriving for a hostname means resolving it a second time,
// the work B6's memo exists to avoid). blockPrivate (the only liftable class)
// gets the four-sentence remedy verbatim; every other class gets
// neverLiftableDetail. A LITERAL host needs no carried kind — it re-derives
// and ignores the parameter.
func literalIPDenialDetail(host string, port int, pol *Policy, kind blockKind) string {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return ""
	}
	ip := net.ParseIP(h)
	if ip == nil {
		if never := neverLiftableDetail("this host's address", kind); never != "" {
			return never
		}
		// The join site for the four DRAFT sentences above: cause, remedy,
		// hint, lifetime. Any owner ruling is an edit to this expression and
		// the constant it names, nowhere else.
		return egressPrivateRangeCause + "; " + egressInternalHostsRemedy + ". " +
			siteInternalHostsCIDRHint + " " + egressDenialSuffix
	}
	if pol != nil {
		if _, denied := pol.deniedExact[ip.String()]; denied {
			return "literal IP " + ip.String() + " is on denied_domains, and a deny always beats an allow"
		}
		if _, denied := pol.deniedExactPort[hostPortKey(ip.String(), port)]; denied {
			return "literal IP " + ip.String() + " is on denied_domains for this port, and a deny always beats an allow"
		}
	}
	// Only blockPrivate is reachable by an EXACT allowed_domains entry, so
	// telling a 127.0.0.1 / 169.254.169.254 / NAT64 literal to list itself
	// there describes a fix that cannot work. Re-derived here: the address is
	// the host.
	litKind, _ := isBlockedIP(ip)
	if never := neverLiftableDetail("literal IP "+ip.String(), litKind); never != "" {
		return never
	}
	return "literal IP " + ip.String() + " is in a private/reserved range, so only an EXACT allowed_domains entry for that address can reach it — " +
		"it is not listed; an egress_redirects \"to\" pointing at this address adds that entry automatically for the runs it covers"
}

// resolveFailedDetail is egressHeaderDetail's value for a builtin:resolve-failed
// refusal: the proxy never learned an address, so nothing was vetted and
// nothing about policy or the address-range guard explains the deny. A FIXED
// sentence like the ones above, never echoing the host.
//
// Exists so a resolver outage is never labelled builtin:private-ip with
// literalIPDenialDetail's "declare it under internal_hosts" advice attached —
// advice that can't fix a resolver outage and points at loosening an SSRF
// control for a fault that is neither.
const resolveFailedDetail = "this host did not resolve (DNS failure, no such name, or no address records), so no address " +
	"could be vetted; this is a name-resolution fault, not the private-address guard — check the sandbox's resolver, " +
	"not the allowlist"

// writeEgressDeny writes the 403 both forward paths (handlePlain,
// handleConnect) give a DENIED request: the static refusal headers, plus — for
// the one refusal an operator reliably misreads — the cause and where to fix
// it. Every other refusal reason already says all there is to say.
//
// Lives here rather than in proxy.go beside its two callers so it sits with
// the rule it explains (and keeps proxy.go under the 1000-line split gate).
//
// memoed is the CALLER's declaration that this refusal can have come out of the
// per-run private-IP memo — true at the two sites that hand on an evaluate()
// verdict, false at a lane that built the deny log itself.
func (p *Proxy) writeEgressDeny(w http.ResponseWriter, host string, port int, log *egress.DecisionLog, memoed bool) {
	body := "egress denied by policy"
	reason := decisionReason(log)
	// An evaluate() DENY carrying no decision log is B6's memoed private-ip
	// refusal: evaluate() answered an identical repeat out of the per-run
	// memo, so the 403 has to be rebuilt here to stay byte-identical to the
	// first one, retry header included.
	//
	// TWO facts, both required, neither inferred from the other: the CALLER
	// says the verdict could have come from the memo (memoed), and the MEMO
	// says it did for this host:port. policy_test.go pins both negatives and
	// the positive.
	if memoed && reason == "" && p.privateIPMemoed(host, port) {
		reason = "builtin:private-ip"
	}
	switch reason {
	case "builtin:private-ip":
		w.Header().Set(egressHeaderRetry, egressRetryNever)
		if detail := literalIPDenialDetail(host, port, p.policy, p.privateIPBlockKind(host, port)); detail != "" {
			w.Header().Set(egressHeaderDetail, detail)
			body = "egress denied: " + detail
		}
	case "builtin:resolve-failed":
		w.Header().Set(egressHeaderDetail, resolveFailedDetail)
		body = "egress denied: " + resolveFailedDetail
	}
	setEgressRefusalHeadersWithReason(w, egressRefusalDenied, host, reason)
	if policyDecidedReason(reason) {
		p.denyAttributed(w, body, http.StatusForbidden)
		return
	}
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
	// kind is WHICH guard class refused — unexported, a proxy-internal
	// classification, carried because only blockPrivate is ever liftable and
	// the 403's detail must say so honestly. Zero (blockNone) on every
	// non-range denial and on every admission.
	kind blockKind
	// Lifted is true when an address isBlockedIP would otherwise deny was
	// admitted only because the caller's lift predicate (vetHostLift) accepted
	// it — an operator-declared internal host. Never true for VetHost.
	Lifted bool
	// Unresolved distinguishes "this proxy could not learn the addresses at
	// all" from "an address is blocked". egressTarget's corp-upstream branch
	// reads it to EXCUSE the case (a sandbox host frequently cannot resolve
	// external names under an operator upstream); its direct-dial branch
	// reads it to ATTRIBUTE the case (errHostUnresolved ->
	// builtin:resolve-failed) — still denied, but a DNS fault, not the
	// address-range guard, with the opposite fix. Every other caller treats
	// Denied as Denied.
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
// exactly like VetHost. Reason strings and the fail-closed shape are unchanged
// from VetHost.
func vetHostLift(host string, res resolver, lift func(net.IP) bool) IPGuardResult {
	host = strings.TrimSuffix(strings.ToLower(host), ".")
	if host == "" {
		return IPGuardResult{Denied: true, Reason: "empty host"}
	}
	admit := func(ip net.IP) (bool, bool, string, blockKind) { // ok, lifted, reason, kind
		kind, why := isBlockedIP(ip)
		if kind == blockNone {
			return true, false, "", blockNone
		}
		if kind == blockPrivate && lift != nil && lift(ip) {
			return true, true, "", blockNone
		}
		return false, false, why, kind
	}
	// Literal IP fast path.
	if ip := net.ParseIP(host); ip != nil {
		ok, lifted, why, kind := admit(ip)
		if !ok {
			return IPGuardResult{Denied: true, Reason: why, kind: kind}
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
		ok, lifted, why, kind := admit(ip)
		if !ok {
			return IPGuardResult{Denied: true, Reason: fmt.Sprintf("blocked address %s: %s", ip, why), kind: kind}
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
// NOT in ipguard.ReservedV6 (which denies wholesale) on purpose: the prefix
// also contains :: and ::1, already named precisely elsewhere, and denying
// all of ::/96 would refuse an address whose embedded v4 is public. Only the
// embedded address decides — see isBlockedIP.
var v4CompatiblePrefix = netip.MustParsePrefix("::/96")

// v4CompatibleEmbeddedV4 returns the IPv4 embedded in the low 32 bits of an
// IPv4-COMPATIBLE ::/96 address, and (nil, false) for anything else —
// including an IPv4-mapped ::ffff:/96 address, which Unmap turns back into the
// IPv4 the canonical path already judges.
//
// Trust boundary: deny-only, like nonCanonicalLiteralIP. Its one consumer
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
// internal/ipguard. Denied regardless of policy: SiteConfig.InternalHosts (the
// one override that exists) cannot reach loopback/link-local/metadata, because
// its CIDRs must lie inside ipguard.Liftable, which never intersects them.
// IPv4-mapped IPv6 addresses are unwrapped so "::ffff:127.0.0.1" cannot smuggle
// a loopback target past the guard.
//
// The returned blockKind is finer than a bool ONLY so vetHostLift can tell
// apart the one liftable case (blockPrivate) from every other, unconditional
// denial. Reason strings are unchanged from before blockKind existed.
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
	// Block the prefix wholesale and re-run the embedded v4 through the v4
	// block check so the reason names the real target. Honest residual: only
	// well-known + local-use NAT64 prefixes are covered; scoped to NAT64
	// prefixes on purpose to avoid false-positiving legit addresses whose low
	// 32 bits happen to fall in a reserved v4 range.
	//
	// 6to4 (2002::/16) is the OTHER embedded-v4 shape and is NOT handled here
	// (its IPv4 sits at bits 16..48, unreadable by this extraction) — it is
	// denied a step earlier, wholesale via ipguard.ReservedV6, reaching this
	// branch as blockReservedOther and never as blockNAT64.
	if embedded, ok := ipguard.NAT64EmbeddedV4(ip); ok {
		if kind, why := isBlockedIP(embedded); kind != blockNone {
			return blockNAT64, "nat64-embedded " + why
		}
		return blockNAT64, "nat64 prefix (RFC 6052/8215)"
	}
	// The IPv4-COMPATIBLE ::/96 form is the THIRD embedded-v4 shape, and the
	// one the predicates above cannot see: net.ParseIP parses "::127.0.0.1"
	// with To4()==nil, IsLoopback/IsLinkLocalUnicast false, and no ::/96 entry
	// in PrivateReserved — walking past both step 0 and the literal fast
	// path. Re-run the embedded v4 the same way the NAT64 arm does. Only the
	// embedded address decides (the prefix isn't denied wholesale), so this
	// can only ADD denials the canonical spelling already gets.
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
