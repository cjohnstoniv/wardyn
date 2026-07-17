// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package transport provides a hardened, governed HTTP client for the composer's
// OWN outbound LLM API egress — the control-plane calls each networked composer
// backend makes to a third-party model provider (Anthropic, OpenAI, Azure,
// Bedrock, …). Those calls send the operator's prompt plus grounded repo/remote
// context to an external model, yet — unlike the data-plane agent egress — they
// originate from wardynd itself and are therefore NOT subject to Wardyn's
// per-workspace egress sidecar (internal/egress). This package closes that gap.
//
// The returned *http.Client enforces, on every dial:
//
//   - SSRF guard: the target host is resolved and the connection is refused if
//     ANY resolved address falls in a private / loopback / link-local / cloud-
//     metadata range (mirrors internal/egress/proxy/policy.go's isBlockedIP /
//     VetHost). The single vetted IP is dialed explicitly, so the transport
//     never re-resolves the hostname — closing the TOCTOU / DNS-rebinding window.
//   - Host allowlist: the connection is refused to any host not on an explicit
//     allowlist of permitted LLM endpoints (the provider's canonical host plus
//     any base host the operator configured).
//   - No ambient proxy: Proxy is nil, so an HTTP(S)_PROXY in the daemon's
//     environment can NOT redirect these calls (nor the API keys they carry).
//   - Redirect guard: a redirect to a non-allowlisted host is refused.
//   - Sensible timeouts bound every request.
//
// It deliberately does NOT import internal/egress — that would create an
// import cycle (egress depends on types, and pulling it in here would entangle
// the composer with the data-plane proxy). The blocked-range CIDR table is
// shared with policy.go via the stdlib-only leaf internal/ipguard instead.
package transport

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

// defaultTimeout bounds a single LLM request (the whole Do, including reading
// the response body). A composer proposal is a small JSON object, so this is
// generous headroom rather than a target.
const defaultTimeout = 120 * time.Second

// Resolver abstracts DNS so the guard can be unit-tested without real network.
type Resolver interface {
	LookupIP(ctx context.Context, host string) ([]net.IP, error)
}

// netResolver is the production resolver backed by the standard library.
type netResolver struct{}

func (netResolver) LookupIP(ctx context.Context, host string) ([]net.IP, error) {
	return net.DefaultResolver.LookupIP(ctx, "ip", host)
}

// Guard vets a dial target against the host allowlist and the blocked-IP ranges.
// It is safe for concurrent use (its fields are read-only after construction).
type Guard struct {
	allowed map[string]struct{}
	// allowPrivate relaxes the loopback / RFC1918 block for an explicitly
	// operator-configured LOCAL endpoint (e.g. an OpenAI-compatible BYOM server
	// such as Ollama or vLLM on 127.0.0.1). Even when true, the link-local /
	// cloud-metadata range (169.254.0.0/16), multicast, and the unspecified
	// address stay blocked — those are never a legitimate LLM and are the prime
	// SSRF target.
	allowPrivate bool
	res          Resolver
}

// NewGuard compiles allowedHosts (exact, case-insensitive host match) into a
// Guard. res may be nil to use the system resolver.
func NewGuard(allowedHosts []string, allowPrivate bool, res Resolver) *Guard {
	if res == nil {
		res = netResolver{}
	}
	m := make(map[string]struct{}, len(allowedHosts))
	for _, h := range allowedHosts {
		if h = normalizeHost(h); h != "" {
			m[h] = struct{}{}
		}
	}
	return &Guard{allowed: m, allowPrivate: allowPrivate, res: res}
}

// normalizeHost lowercases, trims, drops a trailing dot, and strips brackets
// from an IPv6 literal so allowlist comparisons are stable.
func normalizeHost(h string) string {
	h = strings.ToLower(strings.TrimSpace(h))
	h = strings.TrimSuffix(h, ".")
	h = strings.TrimPrefix(h, "[")
	h = strings.TrimSuffix(h, "]")
	return h
}

// HostAllowed reports whether host is on the allowlist (exact, normalized).
func (g *Guard) HostAllowed(host string) bool {
	_, ok := g.allowed[normalizeHost(host)]
	return ok
}

// Vet validates host and returns the single vetted IP that MUST be dialed. It
// fails closed: a host that is not on the allowlist, fails to resolve, resolves
// to no addresses, or resolves to ANY blocked address is refused. A mix of
// public and blocked answers is the classic DNS-rebinding shape and is refused
// wholesale.
func (g *Guard) Vet(ctx context.Context, host string) (net.IP, error) {
	h := normalizeHost(host)
	if h == "" {
		return nil, fmt.Errorf("transport: empty dial host")
	}
	if !g.HostAllowed(h) {
		return nil, fmt.Errorf("transport: host %q is not in the LLM egress allowlist", host)
	}
	// Literal IP: vet directly, no DNS.
	if ip := net.ParseIP(h); ip != nil {
		if blocked, why := IsBlocked(ip, g.allowPrivate); blocked {
			return nil, fmt.Errorf("transport: refusing to dial %s: %s", ip, why)
		}
		return ip, nil
	}
	ips, err := g.res.LookupIP(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("transport: resolving %q failed: %w", host, err)
	}
	if len(ips) == 0 {
		return nil, fmt.Errorf("transport: %q resolved to no addresses", host)
	}
	for _, ip := range ips {
		if blocked, why := IsBlocked(ip, g.allowPrivate); blocked {
			return nil, fmt.Errorf("transport: refusing %q: resolved address %s is %s", host, ip, why)
		}
	}
	return ips[0], nil
}

// blockedV4 / blockedV6 are the unconditionally-denied CIDR ranges, shared
// with the egress proxy via internal/ipguard (loopback, link-local, multicast,
// and the unspecified address are handled separately via the net.IP predicates
// so they can stay blocked even under allowPrivate).
var (
	blockedV4 = ipguard.PrivateReservedV4
	blockedV6 = ipguard.UniqueLocalV6
)

// IsBlocked reports whether ip is in an unconditionally-denied range. IPv4-mapped
// IPv6 addresses are unwrapped first so a "::ffff:127.0.0.1" cannot smuggle a
// loopback (or metadata) target past the guard. When allowPrivate is true the
// loopback and RFC1918 ranges are permitted (an explicitly operator-configured
// local endpoint), but the link-local / cloud-metadata range, multicast, and the
// unspecified address remain blocked.
func IsBlocked(ip net.IP, allowPrivate bool) (bool, string) {
	if ip == nil {
		return true, "nil ip"
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4 // unwrap IPv4-mapped IPv6
	}
	// Always blocked, even under allowPrivate: link-local unicast covers the
	// 169.254.169.254 cloud-metadata address (169.254.0.0/16).
	if ip.IsUnspecified() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true, "link-local/metadata/multicast/unspecified"
	}
	// NAT64-embedded IPv4 smuggling: 64:ff9b::a9fe:a9fe reaches 169.254.169.254
	// while To4()==nil lets it slip every check below. Block the prefix wholesale
	// and re-run the embedded v4 through this same guard (BEFORE the allowPrivate
	// bypass, so a NAT64-smuggled metadata target stays blocked even under it,
	// while a NAT64-embedded RFC1918 target follows allowPrivate like a direct one).
	if embedded, ok := ipguard.NAT64EmbeddedV4(ip); ok {
		if blocked, why := IsBlocked(embedded, allowPrivate); blocked {
			return true, "nat64-embedded " + why
		}
		return true, "nat64 prefix (RFC 6052/8215)"
	}
	if allowPrivate {
		return false, ""
	}
	if ip.IsLoopback() { // 127.0.0.0/8 and ::1
		return true, "loopback"
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
	return false, ""
}

// NewClient returns a hardened *http.Client whose every connection is vetted by
// a Guard built from allowedHosts. allowPrivate relaxes the loopback / RFC1918
// block for an explicitly operator-configured local endpoint (see Guard). A
// timeout <= 0 uses a sensible default.
func NewClient(allowedHosts []string, allowPrivate bool, timeout time.Duration) *http.Client {
	return clientForGuard(NewGuard(allowedHosts, allowPrivate, nil), timeout)
}

// clientForGuard builds the hardened client around g (split out so tests can
// inject a Guard with a fake resolver).
func clientForGuard(g *Guard, timeout time.Duration) *http.Client {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		// Do NOT honor HTTP(S)_PROXY from the environment: these control-plane
		// calls (and the API keys they carry) must never be redirected through an
		// ambient proxy.
		Proxy: nil,
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(addr)
			if err != nil {
				return nil, fmt.Errorf("transport: malformed dial address %q: %w", addr, err)
			}
			ip, err := g.Vet(ctx, host)
			if err != nil {
				return nil, err
			}
			// Dial the vetted IP explicitly so the transport never re-resolves the
			// hostname (TOCTOU / DNS-rebinding guard). TLS still uses the original
			// hostname for ServerName (http.Transport sets it from the request
			// URL), so certificate verification is unaffected.
			return dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		},
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   4,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	return &http.Client{
		Transport: tr,
		Timeout:   timeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if !g.HostAllowed(req.URL.Hostname()) {
				return fmt.Errorf("transport: refusing redirect to non-allowlisted host %q", req.URL.Host)
			}
			if len(via) >= 10 {
				return fmt.Errorf("transport: stopped after 10 redirects")
			}
			return nil
		},
	}
}

// HostOf returns the normalized hostname (no port) of rawURL, or "" when rawURL
// is empty or unparseable. An empty result yields an empty allowlist, which
// fails closed (every host refused).
func HostOf(rawURL string) string {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return ""
	}
	return normalizeHost(u.Hostname())
}
