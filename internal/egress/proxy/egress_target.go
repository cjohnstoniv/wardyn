// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// Resolving a vetted forward-egress dial target: the corp-upstream branch and
// its operator-declared bypass list (SiteConfig.UpstreamProxyNoProxy), the
// operator-declared internal-host SSRF-guard lift (SiteConfig.InternalHosts),
// and the operator-configured internal model gateway's own per-request vet
// (vetTrustedHost). This is the ONE decision every forward-egress caller
// shares.
//
// Composed here: the guard runs on BOTH branches and InternalHosts is what
// lifts it on either. Under the corp upstream this file resolves the name for the guard
// alone, and on a lift stamps site-config:internal-host on the HOSTNAME it
// then hands the corp proxy — which still has to be able to dial that
// address, since the guard binds the name at CHECK time only — the corp
// proxy resolves again for the dial. With the bypass, the dial is made
// locally instead, where the same guard denies RFC 6598 unless InternalHosts
// lifts it. Bypass routes, InternalHosts admits.

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// ruleSourceInternalHost attributes an allow to the operator-declared
	// SiteConfig.InternalHosts lift of the private/reserved-IP guard.
	ruleSourceInternalHost = "site-config:internal-host"
	// ruleSourceEgressRedirect attributes an allow to the operator-authored
	// EXACT literal-IP allow entry that trusted a private/reserved address,
	// so an egress redirect's grant is visible in the trail rather than
	// reading as an ordinary policy:allowed. An operator who hand-pastes the
	// same literal into allowed_domains gets the same label.
	ruleSourceEgressRedirect = "site-config:egress-redirect"
)

// errHostUnresolved is egressTarget's sentinel for the three outcomes that
// are NOT private-IP blocks — a resolver outage, NXDOMAIN, and a zero-answer
// lookup — so a caller can attribute them honestly instead of folding them
// into builtin:private-ip: the two denials have OPPOSITE fixes (site config
// vs. the sandbox's resolver), and audited as the first an operator would
// widen an SSRF control over a DNS outage. Wrapped, never returned bare, so
// the vet's own Reason still reaches the log.
var errHostUnresolved = errors.New("proxy: host did not resolve")

// hostBlockedError is egressTarget's address-range refusal, carrying WHICH
// guard class fired — only blockPrivate is liftable by
// SiteConfig.InternalHosts, so writeEgressDeny needs to tell an operator
// whose target is unreachable forever apart from one that is one site-config
// line away. A struct rather than a sentinel per class so errors.As reads the
// field without a switch over five sentinels.
type hostBlockedError struct {
	kind blockKind
	msg  string
}

func (e *hostBlockedError) Error() string { return e.msg }

// blockKindOf reports the guard class behind err, or blockNone when err is not a
// range refusal (a resolver fault, a policy deny, anything else).
func blockKindOf(err error) blockKind {
	var hb *hostBlockedError
	if errors.As(err, &hb) {
		return hb.kind
	}
	return blockNone
}

// egressTarget resolves host:port to the dial target a forward-egress call
// site should use for THIS proxy's mode, so every forward-egress caller
// (evaluate, serveMITMRequest, handleGitBroker, handleGitPATBroker) makes the
// SAME choice instead of each re-deriving it. With an operator upstream
// configured, the corp proxy — not this process — resolves and dials, so the
// target is the real HOSTNAME:port sent by name; otherwise the full local
// private-IP-guarded resolve+pin (Proxy.vetHost) applies as always.
//
// This does NOT special-case a configured LLM gateway host: gatewayTarget is
// the ONLY place a gateway gets vetTrustedHost's relaxed vet, and only for
// the brokered /wardyn/llm/* route. Folding that in here would lift the
// private-IP guard for the gateway hostname on every forward-egress path.
//
// ruleSource is "" except "site-config:internal-host" or
// "site-config:egress-redirect"; only evaluate() consumes it.
func (p *Proxy) egressTarget(host string, port int) (target, ruleSource string, err error) {
	// The operator's OWN exactly-allowed literal is answered FIRST, before
	// the upstream branch and the vet, since it has no hostname behind it to
	// rebind. ABOVE the upstream branch, not below it, so every re-vet path
	// that reaches here without going through evaluate (serveMITMRequest,
	// the two brokers) does not hard-deny the same address evaluate()
	// already allowed. It only ADDS an admission: trustsExactLiteralIP gates
	// on blockPrivate AND onOwnSubnetOrControlPlane, so loopback,
	// link-local, metadata, NAT64 and this proxy's own network are refused
	// however they are allow-listed, and deny still beats allow.
	if ip := net.ParseIP(strings.TrimSuffix(strings.ToLower(host), ".")); ip != nil && p.trustsExactLiteralIP(ip, port) {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), ruleSourceEgressRedirect, nil
	}
	// Upstream-first: with a corporate upstream configured, EVERY forward
	// dial is CONNECTed through it UNLESS the operator declared this
	// destination on the upstream's bypass list, decided HERE once for every
	// forward-egress caller. A bypassed host falls THROUGH to p.vetHost
	// below like an unproxied dial: the bypass changes which hop dials,
	// never what the SSRF guard permits, and grants no policy allow.
	if p.upstream != nil && !p.bypassUpstream(host) {
		// The one thing the upstream hop cannot be trusted to re-derive: a
		// NON-CANONICAL literal (inet_aton spelling, zone-suffixed IPv6) that
		// net.ParseIP refuses, so evaluate's step-0 guard never saw it, and
		// the corp proxy's own parser would turn it back into
		// loopback/link-local/metadata. Canonical literals are NOT re-vetted
		// here since evaluate has already decided them.
		if ip := nonCanonicalLiteralIP(host); ip != nil {
			if kind, why := isBlockedIP(ip); kind != blockNone {
				return "", "", &hostBlockedError{kind: kind,
					msg: fmt.Sprintf("host %q denied: non-canonical literal for %s: %s", host, ip, why)}
			}
		}
		// The corp proxy dials, but the SSRF guard still binds the NAME:
		// resolve HERE for the guard only, and keep sending the HOSTNAME
		// onward. Two stated residuals (THREAT-MODEL.md §4.2): a name only
		// the corp proxy can resolve is left to its own egress controls
		// (Unresolved does not deny), and the guard binds the name at CHECK
		// time only — a name that answers differently to the two resolvers
		// is not bound at dial time, unlike the direct-dial path below.
		if guard := p.vetHost(host); guard.Denied && !guard.Unresolved {
			return "", "", &hostBlockedError{kind: guard.kind, msg: fmt.Sprintf("host %q denied: %s", host, guard.Reason)}
		} else if guard.Lifted {
			ruleSource = ruleSourceInternalHost
		}
		return net.JoinHostPort(host, strconv.Itoa(port)), ruleSource, nil
	}
	guard := p.vetHost(host)
	if guard.Denied {
		// Unresolved still DENIES here (fail closed: a direct dial has no
		// second resolver to defer to), but as itself, so evaluate can audit
		// it as a resolver fault instead of the private-address guard.
		if guard.Unresolved {
			return "", "", fmt.Errorf("host %q: %s: %w", host, guard.Reason, errHostUnresolved)
		}
		return "", "", &hostBlockedError{kind: guard.kind, msg: fmt.Sprintf("host %q denied: %s", host, guard.Reason)}
	}
	if guard.Lifted {
		ruleSource = ruleSourceInternalHost
	}
	return net.JoinHostPort(guard.IP.String(), strconv.Itoa(port)), ruleSource, nil
}

// trustsExactLiteralIP reports whether ip — a bare literal an agent or a
// redirect named — may skip the SSRF guard because the operator declared
// that EXACT address in an AllowedDomains entry. Only blockPrivate
// (RFC1918/ULA/CGNAT) qualifies, the same ceiling SiteConfig.InternalHosts
// lifts: loopback, link-local/metadata, NAT64 and other reserved ranges are
// NEVER trusted even when explicitly allow-listed. Deny still beats allow.
//
// onOwnSubnetOrControlPlane is refused HERE too, for the same reason
// liftInternalHost refuses it: without this a literal on the proxy's own
// subnet was trusted straight through while the hostname spelling of the
// same address was denied.
func (p *Proxy) trustsExactLiteralIP(ip net.IP, port int) bool {
	kind, _ := isBlockedIP(ip)
	return kind == blockPrivate && !p.onOwnSubnetOrControlPlane(ip) &&
		p.policy != nil && p.policy.AllowsLiteralIP(ip.String(), port)
}

// bypassUpstream reports whether a dial to host must SKIP the corporate
// upstream proxy and be made directly — SiteConfig.UpstreamProxyNoProxy, the
// operator-hop equivalent of NO_PROXY. host may be a hostname (matched by label
// suffix) or a literal IP (matched against a declared CIDR).
//
// It answers a ROUTING question only. It never lifts the private-IP SSRF guard
// (a bypassed dial runs p.vetHost like any unproxied one) and never grants a
// policy allow (evaluate() has already decided that). Nil/empty list => false
// for every host, i.e. byte-identical to before the field existed.
//
// Consulted at four sites, all keyed on the REAL destination host: egressTarget
// (which hop resolves+vets), gatewayTarget (the brokered-LLM route), the
// egressDial closure (the forwarding transport's dial) and handleConnect (the
// opaque tunnel's dial). Those are every place the upstream/direct choice is
// made.
func (p *Proxy) bypassUpstream(host string) bool {
	if len(p.noProxy) == 0 {
		return false
	}
	return noProxyRulesCoverHost(p.noProxy, host)
}

// noProxyRulesCoverHost is bypassUpstream's matching rule, factored out so it
// has exactly ONE spelling: Config.applyDefaultsAndValidate's boot-time
// "AWS SSO injection host not covered by the bypass list" warning consults
// the same compiled rules through this function, rather than keeping a
// second copy that could silently drift from what bypassUpstream actually
// does at dial time.
func noProxyRulesCoverHost(rules []noProxyRule, host string) bool {
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return false
	}
	ip := net.ParseIP(h)
	for _, r := range rules {
		if r.cidr != nil {
			if ip != nil && r.cidr.Contains(ip) {
				return true
			}
			continue
		}
		if h == r.suffix || strings.HasSuffix(h, "."+r.suffix) {
			return true
		}
	}
	return false
}

// noProxyRule is one compiled SiteConfig.UpstreamProxyNoProxy entry: either a
// CIDR (matched against a literal-IP destination) or a lowercased host/domain
// suffix (matched by label suffix, never mid-label). Exactly one is set.
type noProxyRule struct {
	suffix string
	cidr   *net.IPNet
}

// normalizeNoProxyEntry is the ONE spelling rule for a bypass entry, shared by
// the write-time validator (ValidNoProxyEntry, called from internal/api and
// from Config.applyDefaultsAndValidate) and the compiler below, so the two can
// never disagree about what an operator wrote. A leading "." — the NO_PROXY
// spelling of a domain suffix — is stripped: ".corp.internal" and
// "corp.internal" mean the same thing here, as they do in every NO_PROXY
// implementation.
func normalizeNoProxyEntry(raw string) string {
	e := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), ".")
	return strings.TrimPrefix(e, ".")
}

// noProxyHostRE is the charset a bypass HOST/domain-suffix entry must match. It
// deliberately admits no "*": NO_PROXY's bypass-everything wildcard is spelled
// here by clearing upstream_proxy_url, and one character must never be able to
// un-chain an estate's whole egress.
var noProxyHostRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)

// ValidNoProxyEntry reports whether one SiteConfig.UpstreamProxyNoProxy entry
// is a CIDR or a host/domain suffix this proxy will actually honour. Exported
// because the write-time validator in internal/api must refuse exactly what
// compileNoProxy would drop — a typo'd entry that silently means "still
// proxied" is the failure mode the field exists to end.
func ValidNoProxyEntry(raw string) bool {
	e := normalizeNoProxyEntry(raw)
	if e == "" {
		return false
	}
	if _, _, err := net.ParseCIDR(e); err == nil {
		return true
	}
	return noProxyHostRE.MatchString(e)
}

// noProxyStrings renders the COMPILED rules back for the boot log, so what is
// logged is what is actually in force — never the raw config, which may hold a
// dropped entry the operator then believes is bypassed.
func noProxyStrings(rules []noProxyRule) []string {
	out := make([]string, 0, len(rules))
	for _, r := range rules {
		if r.cidr != nil {
			out = append(out, r.cidr.String())
			continue
		}
		out = append(out, r.suffix)
	}
	return out
}

// compileNoProxy compiles the bypass list, dropping anything ValidNoProxyEntry
// refuses rather than widening the list.
func compileNoProxy(entries []string) []noProxyRule {
	out := make([]noProxyRule, 0, len(entries))
	for _, raw := range entries {
		if !ValidNoProxyEntry(raw) {
			continue
		}
		e := normalizeNoProxyEntry(raw)
		if _, n, err := net.ParseCIDR(e); err == nil {
			out = append(out, noProxyRule{cidr: n})
			continue
		}
		out = append(out, noProxyRule{suffix: e})
	}
	return out
}

// llmUpstream reports the (host, port, prefix) a brokered LLM route should
// dial for vendor's public host (e.g. "api.anthropic.com"): the configured
// internal gateway when one exists, or (vendor, 443, "") unset — the
// byte-identical-to-today default.
func (p *Proxy) llmUpstream(vendor string) (host string, port int, prefix string) {
	if u, ok := p.llmUpstreams[vendor]; ok {
		return u.host, u.port, u.prefix
	}
	return vendor, 443, ""
}

// errGatewayVet is vetTrustedHost's sentinel refusal — distinguished from an
// ordinary SSRF denial so the brokered LLM route can log
// ruleSourceGatewayVetFailed (a GUARD refusal of the operator's own configured
// gateway host) instead of the misleading "brokered:llm" allow-shaped source.
var errGatewayVet = errors.New("proxy: configured gateway host refused")

const (
	// ruleSourceGatewayVetFailed marks llmRouteTarget's refusal of the
	// OPERATOR'S OWN configured LLM gateway — vetTrustedHost's errGatewayVet,
	// above: the gateway host resolved to loopback/link-local/this proxy's own
	// control-plane network, or did not resolve at all. Kept separate from
	// "builtin:dial-failed" (the three genuine network-lost-it dials) so a
	// config problem the operator's own gateway can never satisfy is excluded
	// from wardyn_egress_denies_total alongside failures the network caused —
	// see isPolicyDeny (internal/api/metrics.go).
	ruleSourceGatewayVetFailed = "builtin:gateway-vet-failed"
	// ruleSourceUpstreamProtocolMismatch marks a round trip that GOT AN ANSWER
	// — an HTTP/2 frame on a connection that negotiated no ALPN — which this
	// proxy could not complete over HTTP/2 either: its body could not be sent
	// again, or the HTTP/2 attempt failed too (roundTripUpstream,
	// upstream_protocol.go). Kept separate from "builtin:dial-failed" for the
	// same reason as above, but the opposite direction of unfairness: an
	// identical retry of this request does not fix it, so it counts as a
	// denial (isPolicyDeny does not exclude it) rather than
	// hiding behind the network-fault series an operator might reasonably
	// expect to clear on its own.
	ruleSourceUpstreamProtocolMismatch = "builtin:upstream-protocol-mismatch"
)

// gatewayTarget resolves the dial target for the BROKERED LLM route only
// (proxyLLMRequest) — never for evaluate/serveMITMRequest/the git+PAT
// brokers, which all resolve an ordinary host through egressTarget's
// SSRF-guarded p.vetHost like any other target. Upstream-first, same ceiling
// as egressTarget: with a corp upstream configured the gateway is CONNECTed
// through it by hostname (the transport resolves+dials, so there is no local
// resolve to vet); otherwise the gateway gets vetTrustedHost's relaxed
// per-request resolve+pin — it is CONTROL-PLANE-authored (the operator typed
// it at boot), so no InternalHosts declaration is needed for it.
// The upstream bypass applies here too, for the same reason it applies to an
// ordinary host: an internal model gateway a corp proxy cannot CONNECT to is
// exactly the destination the operator declared on the bypass list.
func (p *Proxy) gatewayTarget(host string, port int) (string, error) {
	if p.upstream != nil && !p.bypassUpstream(host) {
		return net.JoinHostPort(host, strconv.Itoa(port)), nil
	}
	return p.vetTrustedHost(host, port)
}

// vetTrustedHost is gatewayTarget's own-proxy resolver: a CONTROL-PLANE-
// authored LLMUpstreams host (the operator typed it at boot) gets a
// per-request resolve+pin like resolveTrustedURL, PLUS the refusal
// resolveTrustedURL deliberately lacks — refuse if ANY answer is loopback/
// link-local/unspecified/multicast/NAT64-embedded (RFC1918/CGNAT is fine;
// that is the whole point of an internal gateway) OR lands on this proxy's
// own interface subnets/control-plane host (onOwnSubnetOrControlPlane — the
// sidecar shares its control-plane network with postgres/dex/registry, and a
// gateway resolving there must not reach them). Policy.AllowsLiteralIP does
// not apply here (that is evaluate() step 0's mechanism, for an agent-chosen
// target); this is a different trust class entirely. Resolution failure or a
// refused answer returns errGatewayVet — the caller's request 502s; the run
// is otherwise unaffected.
func (p *Proxy) vetTrustedHost(host string, port int) (string, error) {
	h := strings.TrimSuffix(strings.ToLower(host), ".")
	if ip := net.ParseIP(h); ip != nil {
		if ipguard.GatewayIPRefused(ip) || p.onOwnSubnetOrControlPlane(ip) {
			return "", errGatewayVet
		}
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), nil
	}
	res := p.res
	if res == nil {
		res = netResolver{}
	}
	ips, err := res.LookupIP(h)
	if err != nil || len(ips) == 0 {
		return "", errGatewayVet
	}
	for _, ip := range ips {
		if ipguard.GatewayIPRefused(ip) || p.onOwnSubnetOrControlPlane(ip) {
			return "", errGatewayVet
		}
	}
	return net.JoinHostPort(ips[0].String(), strconv.Itoa(port)), nil
}

// vetHost is VetHost plus the operator-declared internal-host exception: an
// address that VetHost would deny ONLY for being private/reserved (RFC1918/
// ULA/CGNAT — never loopback/link-local/metadata/unspecified/multicast/NAT64,
// which vetHostLift never offers to lift) is admitted when host matches a
// declared suffix AND the address falls inside that entry's CIDRs (no CIDRs =
// the full ipguard.Liftable set) AND the address is not one of this proxy's
// own interface subnets or its control-plane host. EVERY answer of a resolved
// hostname must still be admissible (vetHostLift's rebinding rule is
// unchanged) — a host with one liftable and one non-liftable answer is denied.
func (p *Proxy) vetHost(host string) IPGuardResult {
	normalized := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	return vetHostLift(host, p.res, func(ip net.IP) bool {
		return p.liftInternalHost(normalized, ip)
	})
}

// liftInternalHost reports whether ip may be lifted for host under a declared
// SiteConfig.InternalHosts entry. host is already normalized (lowercased,
// trailing dot trimmed) by vetHost.
func (p *Proxy) liftInternalHost(host string, ip net.IP) bool {
	if p.onOwnSubnetOrControlPlane(ip) {
		return false
	}
	for _, r := range p.internalHosts {
		if host != r.suffix && !strings.HasSuffix(host, "."+r.suffix) {
			continue
		}
		if len(r.cidrs) == 0 {
			// No CIDRs declared: the full liftable set (isBlockedIP already
			// guarantees ip is RFC1918/ULA/CGNAT here — kind == blockPrivate).
			return true
		}
		for _, c := range r.cidrs {
			if c.Contains(ip) {
				return true
			}
		}
	}
	return false
}

// onOwnSubnetOrControlPlane reports whether ip is one of this proxy's own
// interface subnets or one of its resolved control-plane addresses — all
// captured once at construction (NewServer). The sidecar shares its
// control-plane network with postgres/dex/registry (docker-compose), so an
// internal-host declaration must never let a run reach the proxy's own network
// neighbors.
//
// TRUST BOUNDARY: this is the CLAMP on both admin-authored exceptions to
// the private-IP guard — liftInternalHost and trustsExactLiteralIP — and on the
// gateway's own vet (vetTrustedHost). Its inputs are captured best-effort at
// startup, and when that capture FAILED an empty clamp silently answered "no"
// for every address, which makes the exceptions fire MORE widely rather than
// less. On Kubernetes the pod's own interface does not carry the wardynd
// ClusterIP, so there the control-plane answers are the ONLY thing standing
// between a declared internal host (or a redirect literal) and the control
// plane. exclusionUnknown therefore answers "yes" for every address — refusing
// every lift and every trust — the fail-closed direction this clamp requires.
func (p *Proxy) onOwnSubnetOrControlPlane(ip net.IP) bool {
	if p.exclusionUnknown {
		return true
	}
	for _, cp := range p.controlPlaneIPs {
		if cp.Equal(ip) {
			return true
		}
	}
	for _, n := range p.localSubnets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// parseInternalHostCIDRs parses a SiteConfig.InternalHosts entry's CIDR
// list, failing the WHOLE entry (ok=false) the moment one fails to parse —
// never returning a partial list. liftInternalHost treats zero CIDRs as "no
// CIDRs declared" and lifts the FULL liftable set for the suffix, so
// silently dropping only the one bad CIDR out of several would widen an
// entry meant to be narrow into that full-set default, the opposite of what
// a parse failure should do.
func parseInternalHostCIDRs(raw []string) (cidrs []*net.IPNet, ok bool) {
	cidrs = make([]*net.IPNet, 0, len(raw))
	for _, c := range raw {
		_, n, err := net.ParseCIDR(c)
		if err != nil {
			return nil, false
		}
		cidrs = append(cidrs, n)
	}
	return cidrs, true
}

// compileInternalHosts compiles each declared internal host: lowercase + trim the suffix (same
// normalization VetHost applies to the request host, so the comparison in
// liftInternalHost is exact), parse its CIDRs (already validated at
// site-config write time and at proxy Config load) via
// parseInternalHostCIDRs, which drops the WHOLE entry on a parse failure.
func compileInternalHosts(hosts []types.InternalHost) []internalHostRule {
	out := make([]internalHostRule, 0, len(hosts))
	for _, h := range hosts {
		suffix := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h.HostSuffix)), ".")
		if suffix == "" {
			continue
		}
		cidrs, ok := parseInternalHostCIDRs(h.CIDRs)
		if !ok {
			continue
		}
		out = append(out, internalHostRule{suffix: suffix, cidrs: cidrs})
	}
	return out
}
