// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// Resolving a vetted forward-egress dial target: the corp-upstream branch and
// its operator-declared bypass list (SiteConfig.UpstreamProxyNoProxy), the
// operator-declared internal-host SSRF-guard lift (SiteConfig.InternalHosts),
// and the operator-configured internal model gateway's own per-request vet
// (vetTrustedHost). Split out of proxy.go at the 1000-line gate — a real seam
// (this is the ONE decision every forward-egress caller shares), not a size
// dodge.
//
// THE COMPOSITION worth holding in one place: the guard runs on BOTH branches
// and InternalHosts is what lifts it on either. Under the corp upstream this
// file resolves the name for the guard alone, denies an answer in a blocked
// range, and on a lift stamps site-config:internal-host on the HOSTNAME it then
// hands the corp proxy — which still has to be able to dial that address, and
// on the private-endpoint estates this branch exists for it cannot. With the
// bypass the dial is made locally instead, where the same unconditional
// private/reserved-IP guard denies RFC 6598 — and InternalHosts is what lifts
// THAT. So on such an estate the two fields are one configuration: Bypass
// routes, InternalHosts admits.

import (
	"errors"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

const (
	// ruleSourceInternalHost attributes an allow to the operator-declared
	// SiteConfig.InternalHosts lift of the private/reserved-IP guard.
	ruleSourceInternalHost = "site-config:internal-host"
	// ruleSourceEgressRedirect attributes an allow to the operator-authored
	// EXACT literal-IP allow entry that trusted a private/reserved address —
	// the grant an egress redirect makes, so it is visible in the trail rather
	// than reading as an ordinary policy:allowed.
	//
	// Honest about what it can and cannot tell apart: the proxy sees the
	// compiled allowlist, not the site-config rows behind it, and
	// substituteArtifactEgress (internal/api) is the only thing in the product
	// that writes a literal IP into a run's allowed_domains — for exactly the
	// runs a redirect is in scope for. An operator who hand-pastes the same
	// literal into allowed_domains gets the same label, which is the same grant
	// by hand.
	ruleSourceEgressRedirect = "site-config:egress-redirect"
)

// errHostUnresolved is egressTarget's sentinel for the three outcomes that are
// NOT private-IP blocks — a resolver outage, NXDOMAIN, and a zero-answer lookup
// — so a caller can attribute them honestly instead of folding them into
// builtin:private-ip. Same reason errGatewayVet exists.
//
// It matters because the two denials have OPPOSITE fixes: a private-address
// block is fixed in site config (internal_hosts), a name that never resolved is
// fixed at the sandbox's resolver. Audited as the first, an operator reads a DNS
// outage as an SSRF-guard hit and goes to widen an SSRF control over it.
//
// Wrapped, never returned bare, so the vet's own Reason still reaches the log.
var errHostUnresolved = errors.New("proxy: host did not resolve")

// egressTarget resolves host:port to the dial target a forward-egress call
// site should use for THIS proxy's mode — hiding the corp-upstream branch so
// every forward-egress caller (evaluate, serveMITMRequest, handleGitBroker,
// handleGitPATBroker) makes the SAME choice instead of each re-deriving it. A
// site that forgot the branch (the brokered LLM routes and the git broker
// both did, W19-W19d-3 / W23-S1-4) unconditionally required local DNS +ran the
// full private-IP guard even under an operator upstream — where the sandbox
// host frequently CANNOT resolve external names at all — and then handed the
// corp proxy a resolved IP LITERAL to CONNECT instead of the real hostname.
// With an operator upstream configured, the corp proxy — not this process —
// resolves and dials, so the target is the real HOSTNAME:port sent by name
// (see dialThroughUpstream / egressDial); otherwise the full local
// private-IP-guarded resolve+pin (Proxy.vetHost) applies as always.
//
// This does NOT special-case a configured LLM gateway host: a sandbox that
// names the gateway on an ordinary CONNECT/MITM path is just another host —
// gatewayTarget (proxyLLMRequest's own resolver) is the ONLY place a gateway
// gets vetTrustedHost's relaxed per-request vet, and only for the brokered
// /wardyn/llm/* route the proxy itself dials. Folding that branch in here
// used to lift the private-IP guard for the gateway HOSTNAME on every
// forward-egress path (evaluate, serveMITMRequest), not just the brokered
// route — fixed by removing it.
//
// ruleSource is "" except "site-config:internal-host" when the address was
// admitted only via the internal-host lift, or "site-config:egress-redirect"
// when a literal IP was admitted as an operator-authored exact allow entry;
// only evaluate() consumes it — the other three callers (serveMITMRequest,
// handleGitBroker, handleGitPATBroker) discard it, unaffected.
func (p *Proxy) egressTarget(host string, port int) (target, ruleSource string, err error) {
	// Upstream-first (a stated ceiling, least code): with a corporate upstream
	// configured, EVERY forward dial is CONNECTed through it by the transport,
	// not only by this branch (see egressDial/dialThroughUpstream) — UNLESS the
	// operator declared this destination on the upstream's bypass list, which is
	// decided HERE, once, for every forward-egress caller.
	//
	// A bypassed host deliberately falls THROUGH to p.vetHost below, exactly as
	// an unproxied dial does. That ordering is the safety property: the bypass
	// changes which hop dials, never what the SSRF guard permits, so a bypassed
	// host with no SiteConfig.InternalHosts declaration covering its address is
	// still denied. The bypass also grants no policy allow — evaluate() already
	// ran allow/deny/approval/method before reaching here.
	if p.upstream != nil && !p.bypassUpstream(host) {
		// The one thing the upstream hop cannot be trusted to re-derive: a
		// NON-CANONICAL literal — an inet_aton IPv4 spelling (127.1, 0x7f000001,
		// 2130706433 = 127.0.0.1; 0251.0376.0.1 = 169.254.0.1) or a
		// zone-suffixed IPv6 literal (fe80::1%eth0, fe80::1%25eth0).
		// net.ParseIP refuses all of those, so evaluate's step-0 literal-IP
		// guard never saw them and the string would be forwarded to the corp
		// proxy verbatim — where inet_aton (or the dialer's own zone-aware
		// parser) turns it back into loopback/link-local/metadata.
		// Checked HERE as well as at step 0 because serveMITMRequest and the two
		// brokers reach this function without going through evaluate (F105).
		// Canonical literals are deliberately NOT re-vetted here: evaluate has
		// already decided them, including the operator's egress-redirect trust.
		if ip := nonCanonicalLiteralIP(host); ip != nil {
			if kind, why := isBlockedIP(ip); kind != blockNone {
				return "", "", fmt.Errorf("host %q denied: non-canonical literal for %s: %s", host, ip, why)
			}
		}
		// The corp proxy dials, but the SSRF guard still binds the NAME. Without
		// this the "unconditional" private/loopback/link-local/metadata guard
		// (policy.go's SECURITY INVARIANTS, THREAT-MODEL.md L2) held only for the
		// LITERAL spelling under an upstream: evaluate()'s step 0 guards literals,
		// this branch returned before p.vetHost, and a name the agent controls
		// that resolves to 169.254.169.254 was handed to the corp proxy to resolve
		// and dial for it. So resolve HERE for the guard only, and keep sending
		// the HOSTNAME onward — an upstream that was handed a resolved literal
		// refuses it (W23-S1-4 / W19-W19d-3), which is what this branch exists for.
		//
		// Unresolved is the one denial that does NOT bite: on a private-endpoint
		// estate the sandbox host frequently cannot resolve external names at all,
		// and turning that into a deny would break every upstream deployment. That
		// residual is stated in policy.go's invariants and in THREAT-MODEL.md
		// rather than papered over: under an upstream the guard binds every name
		// this proxy CAN resolve, and a name only the corp proxy can resolve is
		// left to the corp proxy's own egress controls.
		//
		// The SECOND residual, for the same reason the pin is gone: the guard
		// binds the name at CHECK time only — the corp proxy resolves again for
		// the dial, so a name that answers differently to the two resolvers
		// (short-TTL rebinding, or a split-horizon zone only the corp proxy can
		// see) is not bound at dial time. The direct-dial path below closes that
		// by pinning the vetted address; this branch cannot. THREAT-MODEL.md §4.2
		// states both.
		if guard := p.vetHost(host); guard.Denied && !guard.Unresolved {
			return "", "", fmt.Errorf("host %q denied: %s", host, guard.Reason)
		} else if guard.Lifted {
			ruleSource = ruleSourceInternalHost
		}
		return net.JoinHostPort(host, strconv.Itoa(port)), ruleSource, nil
	}
	// A literal IP the operator explicitly allowed EXACTLY is trusted here for
	// the same reason evaluate() step 0 trusts it — an egress-redirect "To" on
	// RFC1918/CGNAT space (substituteArtifactEgress adds that host to
	// allowed_domains for exactly the runs the redirect is in scope for) has no
	// hostname behind it to rebind. Without this, the three callers that re-vet
	// AFTER evaluate() already allowed the request — serveMITMRequest (the
	// token-injecting redirect lane, i.e. the whole point of a corp mirror) and
	// the two brokers — re-derived the same address and hard-denied it, so a
	// redirect to a literal internal address 502'd with "vet failed" even though
	// policy had trusted it. Deny still beats allow (AllowsLiteralIP checks the
	// deny lists first).
	//
	// This only ADDS an admission: a literal that is not an exactly-allowed
	// blockPrivate address falls through to p.vetHost below unchanged (including
	// its own InternalHosts lift), so nothing reachable before becomes
	// unreachable — and a loopback/metadata/NAT64 literal is denied there even
	// when allow-listed, as is one on this proxy's own subnet or its
	// control-plane host (trustsExactLiteralIP gates on blockPrivate AND
	// onOwnSubnetOrControlPlane, the same pair liftInternalHost gates on).
	if ip := net.ParseIP(strings.TrimSuffix(strings.ToLower(host), ".")); ip != nil && p.trustsExactLiteralIP(ip, port) {
		return net.JoinHostPort(ip.String(), strconv.Itoa(port)), ruleSourceEgressRedirect, nil
	}
	guard := p.vetHost(host)
	if guard.Denied {
		// Unresolved is the vet's own "no address was ever learned" — the same
		// field the upstream branch above reads to excuse the case. Here it
		// still DENIES (fail closed: a direct dial has no second resolver to
		// defer to), but as itself, so evaluate can audit it as a resolver
		// fault instead of as the private-address guard.
		if guard.Unresolved {
			return "", "", fmt.Errorf("host %q: %s: %w", host, guard.Reason, errHostUnresolved)
		}
		return "", "", fmt.Errorf("host %q denied: %s", host, guard.Reason)
	}
	if guard.Lifted {
		ruleSource = ruleSourceInternalHost
	}
	return net.JoinHostPort(guard.IP.String(), strconv.Itoa(port)), ruleSource, nil
}

// trustsExactLiteralIP reports whether ip — a bare literal an agent or a
// redirect named — may skip the SSRF guard because the operator declared that
// EXACT address in an AllowedDomains entry. Only blockPrivate (RFC1918/ULA/
// CGNAT) qualifies, the same ceiling SiteConfig.InternalHosts lifts (see
// liftInternalHost): loopback, link-local/metadata, NAT64 and the other
// reserved ranges are NEVER trusted even when explicitly allow-listed, so an
// operator cannot hand the sandbox 169.254.169.254 by typing it into
// allowed_domains. Deny still beats allow — AllowsLiteralIP checks the deny
// lists first.
//
// onOwnSubnetOrControlPlane is refused HERE for the same reason liftInternalHost
// refuses it: this is the SECOND admin-authored exception to the IP guard, and
// an exception that stopped at "is it private?" would hand a run the proxy's own
// docker-network neighbours (Postgres/Dex/registry) — the exact reach the
// InternalHosts lift was written to withhold. The two exceptions are authored by
// the same admin through the same site-config document, so they share the
// ceiling; without this a literal `to` on the sidecar's own subnet (or an
// allowed_domains entry naming the control-plane address) was trusted straight
// through while the hostname spelling of the very same address was denied.
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
	h := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(host)), ".")
	if h == "" {
		return false
	}
	ip := net.ParseIP(h)
	for _, r := range p.noProxy {
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
// ordinary SSRF denial so the brokered LLM route can log "builtin:dial-failed"
// (a per-request resolve failure) instead of the misleading "brokered:llm"
// allow-shaped source.
var errGatewayVet = errors.New("proxy: configured gateway host refused")

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
// TRUST BOUNDARY (F002): this is the CLAMP on both admin-authored exceptions to
// the private-IP guard — liftInternalHost and trustsExactLiteralIP — and on the
// gateway's own vet (vetTrustedHost). Its inputs are captured best-effort at
// startup, and when that capture FAILED an empty clamp silently answered "no"
// for every address, which makes the exceptions fire MORE widely rather than
// less. On Kubernetes the pod's own interface does not carry the wardynd
// ClusterIP, so there the control-plane answers are the ONLY thing standing
// between a declared internal host (or a redirect literal) and the control
// plane. exclusionUnknown therefore answers "yes" for every address — refusing
// every lift and every trust — which is the fail-closed direction NewServer's
// comment always claimed and the code did not have.
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
