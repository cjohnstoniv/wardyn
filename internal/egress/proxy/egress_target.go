// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// Resolving a vetted forward-egress dial target: the corp-upstream branch,
// the operator-declared internal-host SSRF-guard lift (SiteConfig.InternalHosts),
// and the operator-configured internal model gateway's own per-request vet
// (vetTrustedHost). Split out of proxy.go at the 1000-line gate — a real seam
// (this is the ONE decision every forward-egress caller shares), not a size
// dodge.

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

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
// admitted only via the internal-host lift; only evaluate() consumes it — the
// other three callers (serveMITMRequest, handleGitBroker, handleGitPATBroker)
// discard it, unaffected.
func (p *Proxy) egressTarget(host string, port int) (target, ruleSource string, err error) {
	// Upstream-first (a stated ceiling, least code): with a corporate upstream
	// configured, EVERY forward dial is CONNECTed through it by the
	// transport, not only by this branch. See egressDial/dialThroughUpstream.
	if p.upstream != nil {
		return net.JoinHostPort(host, strconv.Itoa(port)), "", nil
	}
	guard := p.vetHost(host)
	if guard.Denied {
		return "", "", fmt.Errorf("host %q denied: %s", host, guard.Reason)
	}
	if guard.Lifted {
		ruleSource = "site-config:internal-host"
	}
	return net.JoinHostPort(guard.IP.String(), strconv.Itoa(port)), ruleSource, nil
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
func (p *Proxy) gatewayTarget(host string, port int) (string, error) {
	if p.upstream != nil {
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
		if trustedGatewayIPRefused(ip) || p.onOwnSubnetOrControlPlane(ip) {
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
		if trustedGatewayIPRefused(ip) || p.onOwnSubnetOrControlPlane(ip) {
			return "", errGatewayVet
		}
	}
	return net.JoinHostPort(ips[0].String(), strconv.Itoa(port)), nil
}

// trustedGatewayIPRefused mirrors api.llmGatewayIPRefused (validated at boot)
// for the proxy's own per-request re-check: loopback/link-local/unspecified/
// multicast/NAT64-embedded are refused; RFC1918/ULA/CGNAT are NOT — an
// internal gateway is expected to live there.
func trustedGatewayIPRefused(ip net.IP) bool {
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsMulticast() || ip.IsUnspecified() {
		return true
	}
	_, isNAT64 := ipguard.NAT64EmbeddedV4(ip)
	return isNAT64
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
// interface subnets or its resolved control-plane host — both captured once at
// construction (NewServer). The sidecar shares its control-plane network with
// postgres/dex/registry (docker-compose), so an internal-host declaration must
// never let a run reach the proxy's own network neighbors.
func (p *Proxy) onOwnSubnetOrControlPlane(ip net.IP) bool {
	if p.controlPlaneIP != nil && p.controlPlaneIP.Equal(ip) {
		return true
	}
	for _, n := range p.localSubnets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
