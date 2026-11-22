// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/ipguard"
)

// The portal fetch layer: every request a portal verifier makes to an identity provider (discovery, keys,
// refreshes and retries) goes through one transport per verifier that pins the destination, refuses redirects,
// checks the resolved address itself and dials the address it checked.

const (
	// portalFetchBodyCap bounds any upstream body.
	portalFetchBodyCap = 1 << 20
	// portalFetchTimeout bounds one request.
	portalFetchTimeout = 10 * time.Second
)

// The coarse causes a fetch failure carries into the 503 answer and the denied audit row.
const (
	portalCauseUnreachable    = "unreachable"
	portalCauseIssuerMismatch = "issuer_mismatch"
	portalCauseKeysOffOrigin  = "keys_off_origin"
	portalCauseAddressRefused = "address_refused"
)

var (
	// portalV4Compatible is ::/96, the deprecated IPv4-compatible form. No identity provider needs it, and the
	// shared guards judge only part of it, so a portal fetch refuses all of it.
	portalV4Compatible = netip.MustParsePrefix("::/96")
	// portalSixToFour is 2002::/16.
	portalSixToFour = netip.MustParsePrefix("2002::/16")
)

// portalFetchError is a refused or failed fetch. It names a cause and never carries upstream text.
type portalFetchError struct {
	cause  string
	detail string
}

func (e *portalFetchError) Error() string {
	if e.detail == "" {
		return "portal fetch: " + e.cause
	}
	return "portal fetch: " + e.cause + ": " + e.detail
}

// portalFetchCause is the cause a fetch error carries, unreachable when it names none.
func portalFetchCause(err error) string {
	var fe *portalFetchError
	if errors.As(err, &fe) {
		return fe.cause
	}
	return portalCauseUnreachable
}

// portalAuthority is the lowercase host and effective port of a URL, 443 when it names none.
func portalAuthority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		port = "443"
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// portalListedAuthority normalises one host[:port] entry of the internal host list.
func portalListedAuthority(entry string) string {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if entry == "" {
		return ""
	}
	host, port, err := net.SplitHostPort(entry)
	if err != nil {
		host, port = strings.Trim(entry, "[]"), "443"
	}
	return net.JoinHostPort(host, port)
}

// portalAddressRefused reports whether a resolved address may never be dialled for a portal fetch.
// Loopback, link-local and metadata, unspecified, multicast and NAT64-embedded addresses are refused for every
// host, as are the ::/96 and 6to4 forms that embed an IPv4. Private and reserved space is refused too, except
// the liftable part of it for a host the operator listed as an internal issuer host.
func portalAddressRefused(ip net.IP, internalHost bool) bool {
	if ip == nil || ipguard.GatewayIPRefused(ip) {
		return true
	}
	if addr, ok := netip.AddrFromSlice(ip); ok {
		addr = addr.Unmap()
		if portalV4Compatible.Contains(addr) || portalSixToFour.Contains(addr) {
			return true
		}
	}
	// Private and reserved space is refused. A listed internal host may sit only in the space an operator may lift
	// (ipguard.InLiftable: RFC 1918, unique-local and CGNAT, never the metadata endpoints), not in Teredo,
	// site-local, test-net or class E space.
	if private, _ := ipguard.PrivateReserved(ip); private {
		return !(internalHost && ipguard.InLiftable(ip))
	}
	return false
}

// portalNet carries the pieces a fetch layer needs from its operator and, in tests, its seams.
type portalNet struct {
	handshakeTimeout time.Duration   // bounds resolution, connect and TLS handshake together; 0 means 10 s
	fetchTimeout     time.Duration   // bounds each actual request (discovery and keys separately); 0 means 10 s
	internalHosts    map[string]bool // listed internal authorities
	internalCA       *x509.CertPool  // system roots plus the operator's bundle, for listed hosts only
	roots            *x509.CertPool  // system roots; nil means the platform's
	lookup           func(ctx context.Context, host string) ([]net.IP, error)
	dial             func(ctx context.Context, network, addr string) (net.Conn, error)
	proxy            *PortalProxy // the daemon proxy route for listed issuer hosts; nil means every fetch is direct
}

// timeout is the bound of one actual request.
func (pn *portalNet) timeout() time.Duration {
	if pn.fetchTimeout > 0 {
		return pn.fetchTimeout
	}
	return portalFetchTimeout
}

// portalTransport is the single transport of one verifier.
type portalTransport struct {
	allowed map[string]bool
	// private says the allowed authority is this registration's own internal address, which the operator listed:
	// the only case a private address may be dialled. A public issuer never has it, listed host or not.
	private bool
	net     *portalNet
	base    *http.Transport
}

func newPortalClient(pn *portalNet, allowed map[string]bool, private bool) *http.Client {
	pt := &portalTransport{allowed: allowed, private: private, net: pn}
	pt.base = &http.Transport{
		Proxy:                  nil, // proxy environment variables never pick the destination
		DialTLSContext:         pt.dialTLS,
		TLSHandshakeTimeout:    pn.timeout(),
		ResponseHeaderTimeout:  pn.timeout(),
		MaxResponseHeaderBytes: 64 << 10,
		IdleConnTimeout:        30 * time.Second,
		MaxIdleConnsPerHost:    1,
	}
	return &http.Client{
		Transport: pt,
		Timeout:   pn.timeout(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return &portalFetchError{cause: portalCauseAddressRefused, detail: "redirect refused"}
		},
	}
}

// RoundTrip applies the authority rule to every request before anything is dialled.
func (pt *portalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || !pt.allowed[portalAuthority(req.URL)] {
		return nil, &portalFetchError{cause: portalCauseAddressRefused, detail: "destination not allowed"}
	}
	return pt.base.RoundTrip(req)
}

// dialTLS resolves the host, checks every address, dials the checked address itself and verifies TLS against
// the roots that apply to that host.
func (pt *portalTransport) dialTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, &portalFetchError{cause: portalCauseAddressRefused, detail: "bad address"}
	}
	authority := net.JoinHostPort(strings.ToLower(host), port)
	if !pt.allowed[authority] {
		return nil, &portalFetchError{cause: portalCauseAddressRefused, detail: "destination not allowed"}
	}
	internal := pt.private
	timeout := pt.net.handshakeTimeout
	if timeout <= 0 {
		timeout = portalFetchTimeout
	}
	// One deadline covers resolution, connect and handshake: net/http does not apply its own to a custom TLS dial
	// and stops waiting for the dial when the request ends.
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ips, err := pt.net.resolve(ctx, host)
	if err != nil || len(ips) == 0 {
		return nil, &portalFetchError{cause: portalCauseUnreachable, detail: "name not resolved"}
	}
	for _, ip := range ips {
		if portalAddressRefused(ip, internal) {
			return nil, &portalFetchError{cause: portalCauseAddressRefused, detail: "address refused"}
		}
	}
	dial := pt.net.dial
	if dial == nil {
		dial = (&net.Dialer{Timeout: portalFetchTimeout}).DialContext
	}
	raw, err := dial(ctx, network, net.JoinHostPort(ips[0].String(), port))
	if err != nil {
		return nil, &portalFetchError{cause: portalCauseUnreachable, detail: "connect failed"}
	}
	roots := pt.net.roots
	if internal && pt.net.internalCA != nil {
		roots = pt.net.internalCA
	}
	_ = raw.SetDeadline(time.Now().Add(timeout))
	conn := tls.Client(raw, &tls.Config{ServerName: host, RootCAs: roots, MinVersion: tls.VersionTLS12, NextProtos: []string{"http/1.1"}})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, &portalFetchError{cause: portalCauseUnreachable, detail: "tls handshake failed"}
	}
	_ = raw.SetDeadline(time.Time{})
	return conn, nil
}

func (pn *portalNet) resolve(ctx context.Context, host string) ([]net.IP, error) {
	if ip := net.ParseIP(host); ip != nil {
		return []net.IP{ip}, nil
	}
	if pn.lookup != nil {
		return pn.lookup(ctx, host)
	}
	addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
	if err != nil {
		return nil, err
	}
	ips := make([]net.IP, 0, len(addrs))
	for _, a := range addrs {
		ips = append(ips, a.IP)
	}
	return ips, nil
}

// portalGetJSON fetches one document and decodes it. The body is capped, and neither the body nor a decoder's
// message (which can quote it) reaches an error: only a status class and a cause do.
func portalGetJSON(ctx context.Context, client *http.Client, rawURL string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return &portalFetchError{cause: portalCauseAddressRefused, detail: "bad url"}
	}
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return &portalFetchError{cause: portalFetchCause(err), detail: portalFetchDetail(err)}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return &portalFetchError{cause: portalCauseUnreachable, detail: fmt.Sprintf("status %dxx", resp.StatusCode/100)}
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, portalFetchBodyCap+1))
	if err != nil {
		return &portalFetchError{cause: portalCauseUnreachable, detail: "read failed"}
	}
	if len(body) > portalFetchBodyCap {
		return &portalFetchError{cause: portalCauseUnreachable, detail: "body too large"}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &portalFetchError{cause: portalCauseUnreachable, detail: "malformed json"}
	}
	return nil
}

// portalFetchDetail is the safe detail of a transport error: ours, or a fixed word.
func portalFetchDetail(err error) string {
	var fe *portalFetchError
	if errors.As(err, &fe) {
		return fe.detail
	}
	return "request failed"
}

// portalDiscovery is what discovery yields: the keys URL as the issuer states it, and the address to fetch it at.
type portalDiscovery struct {
	jwksURL  string
	fetchURL string
}

// portalDiscover reads the discovery document of an issuer and applies the exact issuer rule and the same-origin
// rule on the keys URL. With an internal address, the document and the keys are fetched from it, and the keys URL
// is judged against the public origin first.
func portalDiscover(ctx context.Context, client *http.Client, issuer, internalIssuer string) (portalDiscovery, error) {
	pub, err := url.Parse(issuer)
	if err != nil || pub.Scheme != "https" || pub.Hostname() == "" {
		return portalDiscovery{}, &portalFetchError{cause: portalCauseAddressRefused, detail: "issuer not a https url"}
	}
	base := issuer
	var internal *url.URL
	if internalIssuer != "" {
		internal, err = url.Parse(internalIssuer)
		if err != nil || internal.Scheme != "https" || internal.Hostname() == "" {
			return portalDiscovery{}, &portalFetchError{cause: portalCauseAddressRefused, detail: "internal issuer not a https url"}
		}
		base = internalIssuer
	}
	// The document is read by its exact member names: a differently cased spelling is never a substitute for a
	// missing or contradicting canonical member.
	var doc map[string]json.RawMessage
	if err := portalGetJSON(ctx, client, strings.TrimSuffix(base, "/")+"/.well-known/openid-configuration", &doc); err != nil {
		return portalDiscovery{}, err
	}
	docIssuer, okIssuer := portalClaims(doc).str("issuer")
	docKeys, okKeys := portalClaims(doc).str("jwks_uri")
	if !okIssuer || docIssuer != issuer {
		return portalDiscovery{}, &portalFetchError{cause: portalCauseIssuerMismatch}
	}
	if !okKeys {
		return portalDiscovery{}, &portalFetchError{cause: portalCauseKeysOffOrigin}
	}
	keys, err := url.Parse(docKeys)
	if err != nil || keys.Scheme != "https" || keys.User != nil || keys.Fragment != "" || portalAuthority(keys) != portalAuthority(pub) {
		return portalDiscovery{}, &portalFetchError{cause: portalCauseKeysOffOrigin}
	}
	fetch := *keys
	if internal != nil {
		fetch.Host = internal.Host
	}
	return portalDiscovery{jwksURL: docKeys, fetchURL: fetch.String()}, nil
}

// portalAllowedAuthorities is the set of destinations a verifier may reach: the issuer's authority, or with an
// internal address, that authority only. The second result says private addresses may be dialled there, which
// holds only for a registration that selected an internal address whose exact authority is on the operator's
// list. An internal address that is not listed gets an empty set, so nothing is resolved or dialled for it.
func portalAllowedAuthorities(issuer, internalIssuer string, pn *portalNet) (map[string]bool, bool, error) {
	raw := issuer
	if internalIssuer != "" {
		raw = internalIssuer
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" {
		return nil, false, &portalFetchError{cause: portalCauseAddressRefused, detail: "not a https url"}
	}
	authority := portalAuthority(u)
	if internalIssuer != "" && !pn.internalHosts[authority] {
		return map[string]bool{}, false, nil
	}
	return map[string]bool{authority: true}, internalIssuer != "", nil
}
