// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package imageadmit

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

// ErrRefusedTarget marks a request the registry client will not make: a loopback, link-local or metadata
// address, or a non-https destination. It is the same refusal for a registry, a token realm and a redirect.
var ErrRefusedTarget = errors.New("refused target")

// targetGuard decides whether the client may contact a host. RFC1918 registries stay allowed because internal
// registries are normal; loopback, link-local (the metadata address included), unspecified, multicast and
// NAT64-embedded addresses do not (ipguard.GatewayIPRefused). The fields are a test seam.
type targetGuard struct {
	refuse func(net.IP) bool
	lookup func(ctx context.Context, host string) ([]net.IP, error)
}

func defaultGuard() *targetGuard {
	return &targetGuard{
		refuse: ipguard.GatewayIPRefused,
		lookup: func(ctx context.Context, host string) ([]net.IP, error) {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			ips := make([]net.IP, 0, len(addrs))
			for _, a := range addrs {
				ips = append(ips, a.IP)
			}
			return ips, err
		},
	}
}

// check refuses host when it is, or resolves locally to, a forbidden address. One forbidden answer among
// several refuses the host. A name this machine cannot resolve is an error, except when viaProxy: the
// operator's forward proxy resolves names itself, and what it resolves them to is the operator's boundary.
func (g *targetGuard) check(ctx context.Context, host string, viaProxy bool) error {
	_, err := g.resolve(ctx, host, viaProxy)
	return err
}

// resolve is check that also returns the addresses it approved: the literal address itself, or the
// local answer for a name. It is empty for a name the forward proxy is left to resolve.
func (g *targetGuard) resolve(ctx context.Context, host string, viaProxy bool) ([]net.IP, error) {
	host = strings.Trim(host, "[]")
	if ip := net.ParseIP(host); ip != nil {
		if g.refuse(ip) {
			return nil, fmt.Errorf("%w: %s is a loopback, link-local or metadata address", ErrRefusedTarget, host)
		}
		return []net.IP{ip}, nil
	}
	ips, err := g.lookup(ctx, host)
	if err != nil || len(ips) == 0 {
		if viaProxy {
			return nil, nil
		}
		if err == nil {
			err = errors.New("no addresses")
		}
		return nil, fmt.Errorf("resolving %s: %w", host, err)
	}
	for _, ip := range ips {
		if g.refuse(ip) {
			return nil, fmt.Errorf("%w: %s resolves to %s, a loopback, link-local or metadata address", ErrRefusedTarget, host, ip)
		}
	}
	return ips, nil
}

// control is the dialer Control hook: the last check, on the address actually being connected to.
func (g *targetGuard) control(_, address string, _ syscall.RawConn) error {
	h, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrRefusedTarget, err)
	}
	if ip := net.ParseIP(h); ip == nil || g.refuse(ip) {
		return fmt.Errorf("%w: %s", ErrRefusedTarget, h)
	}
	return nil
}

// guardedTransport clones base and installs the guard.
//   - Direct dials resolve with g.lookup, refuse any forbidden answer, then connect to the checked address, so
//     a name cannot resolve differently between the check and the connection. Control re-checks the address.
//   - The proxy is chosen once per request, in RoundTrip: when base.Proxy returns one, the request's target is
//     checked first and a forbidden one fails before the proxy is told about it. Only that request's dial to
//     that proxy skips the address check (the proxy's address is the operator's); any other dial, including a
//     later direct one to the same host:port, is guarded.
func guardedTransport(base *http.Transport, g *targetGuard) http.RoundTripper {
	tr := base.Clone()
	baseProxy := tr.Proxy
	tr.Proxy = func(req *http.Request) (*url.URL, error) {
		pu, _ := req.Context().Value(proxyChoiceKey{}).(*url.URL)
		return pu, nil
	}
	plain := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	guarded := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second, Control: g.control}
	tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		if pu, _ := ctx.Value(proxyChoiceKey{}).(*url.URL); pu != nil && addr == proxyHostPort(pu) {
			return plain.DialContext(ctx, network, addr)
		}
		h, p, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, err
		}
		ips, err := g.resolve(ctx, h, false)
		if err != nil {
			return nil, err
		}
		var last error
		for _, ip := range ips {
			c, err := guarded.DialContext(ctx, network, net.JoinHostPort(ip.String(), p))
			if err == nil {
				return c, nil
			}
			last = err
		}
		return nil, last
	}
	return &guardedRT{tr: tr, baseProxy: baseProxy, g: g}
}

// proxyChoiceKey carries the proxy selected for one request to the transport's Proxy and DialContext hooks.
type proxyChoiceKey struct{}

type guardedRT struct {
	tr        *http.Transport
	baseProxy func(*http.Request) (*url.URL, error)
	g         *targetGuard
}

func (rt *guardedRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.baseProxy == nil {
		return rt.tr.RoundTrip(req)
	}
	pu, err := rt.baseProxy(req)
	if err != nil {
		return nil, err
	}
	if pu == nil {
		return rt.tr.RoundTrip(req)
	}
	if err := rt.g.check(req.Context(), req.URL.Hostname(), true); err != nil {
		return nil, err
	}
	return rt.tr.RoundTrip(req.WithContext(context.WithValue(req.Context(), proxyChoiceKey{}, pu)))
}

func proxyHostPort(u *url.URL) string {
	if u.Port() != "" {
		return u.Host
	}
	port := map[string]string{"https": "443", "socks5": "1080", "socks5h": "1080"}[u.Scheme]
	if port == "" {
		port = "80"
	}
	return net.JoinHostPort(u.Hostname(), port)
}
