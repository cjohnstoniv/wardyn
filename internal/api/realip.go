// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/audit"
)

// A forwarded chain longer than either bound is not a real proxy path; the
// request keeps its TCP peer.
const (
	maxForwardedHops  = 32
	maxForwardedBytes = 2048
)

// ParseTrustedProxies reads WARDYN_TRUSTED_PROXIES: comma-separated CIDRs.
// A zero-length prefix would trust every peer, which is exactly the forgeable
// header routes.go refuses, so it is an error.
func ParseTrustedProxies(list string) ([]netip.Prefix, error) {
	var out []netip.Prefix
	for _, f := range strings.Split(list, ",") {
		if f = strings.TrimSpace(f); f == "" {
			continue
		}
		p, err := netip.ParsePrefix(f)
		if err != nil {
			return nil, fmt.Errorf("WARDYN_TRUSTED_PROXIES: %q is not a CIDR: %w", f, err)
		}
		if p.Bits() == 0 {
			return nil, fmt.Errorf("WARDYN_TRUSTED_PROXIES: %q trusts every address; list only your proxies", f)
		}
		out = append(out, p.Masked())
	}
	return out, nil
}

// trustedProxyClient replaces r.RemoteAddr with the client a trusted proxy
// reported, and stamps the TCP peer on every audit row the request writes as
// data.proxy_peer. With no trusted proxies configured it is the identity.
func (s *Server) trustedProxyClient(next http.Handler) http.Handler {
	if len(s.cfg.TrustedProxies) == 0 {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if client, ok := forwardedClient(r.RemoteAddr, r.Header.Values("X-Forwarded-For"), s.cfg.TrustedProxies); ok {
			r = r.WithContext(audit.WithProxyPeer(r.Context(), r.RemoteAddr))
			r.RemoteAddr = client
		}
		next.ServeHTTP(w, r)
	})
}

// forwardedClient is the chain rule. Headers count only when the TCP peer is
// trusted; the chain is walked right to left, from the hop the peer itself
// appended, and the first address outside the trusted list is the client.
// Everything left of it was written by that client and is never read. A
// malformed or oversized chain, or one made only of trusted addresses, keeps
// the peer (ok false).
func forwardedClient(peer string, xff []string, trusted []netip.Prefix) (string, bool) {
	host := peer
	if h, _, err := net.SplitHostPort(peer); err == nil {
		host = h
	}
	addr, err := netip.ParseAddr(host)
	if err != nil || !inPrefixes(addr.Unmap(), trusted) || len(xff) == 0 {
		return "", false
	}
	raw := strings.Join(xff, ",")
	hops := strings.Split(raw, ",")
	if len(raw) > maxForwardedBytes || len(hops) > maxForwardedHops {
		return "", false
	}
	parsed := make([]netip.Addr, len(hops))
	for i, h := range hops {
		a, ok := forwardedHop(strings.TrimSpace(h))
		if !ok {
			return "", false
		}
		parsed[i] = a
	}
	for i := len(parsed) - 1; i >= 0; i-- {
		if !inPrefixes(parsed[i], trusted) {
			return parsed[i].String(), true
		}
	}
	return "", false
}

// forwardedHop accepts an address, or an address with a port (some cloud
// gateways append one). A zone, a hostname or "unknown" is malformed.
func forwardedHop(h string) (netip.Addr, bool) {
	a, err := netip.ParseAddr(h)
	if err != nil {
		ap, perr := netip.ParseAddrPort(h)
		if perr != nil {
			return netip.Addr{}, false
		}
		a = ap.Addr()
	}
	if a.Zone() != "" {
		return netip.Addr{}, false
	}
	return a.Unmap(), true
}

func inPrefixes(a netip.Addr, ps []netip.Prefix) bool {
	return slices.ContainsFunc(ps, func(p netip.Prefix) bool { return p.Contains(a) })
}
