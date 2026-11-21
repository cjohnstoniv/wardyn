// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strings"
	"sync"
	"time"
)

// The proxied portal fetch: the discovery and key fetches of an issuer host the operator listed on the daemon
// proxy's always list. wardynd resolves the host itself, refuses unless every answer passes the portal address
// rules, and asks the proxy to connect to the address it checked, never to the name. The issuer is trusted only
// under the roots the operator named for proxied issuers. Each fetch uses its own connection, and every socket
// carries an absolute deadline, because net/http runs connection setup detached from the request.

// PortalProxy sends the discovery and key fetches of listed issuer hosts through the daemon proxy.
type PortalProxy struct {
	URL         *url.URL                        // http, https, socks5 or socks5h; may carry the proxy's credential; never logged
	Listed      func(host string) (bool, error) // whether an issuer host is on the always list, in that list's host form
	ProxyRoots  *x509.CertPool                  // an https proxy's own certificate; nil means the system roots
	IssuerRoots *x509.CertPool                  // the only roots a proxied issuer is trusted under; nil refuses every proxied verifier
}

// The details of a proxied fetch failure. The first three are proxy-side failures before the issuer's TLS
// begins, after which the next checked address is tried.
const (
	portalDetailProxyConnect   = "proxy connect failed"
	portalDetailProxyTarget    = "proxy target failed"
	portalDetailProxyStatus    = "proxy status "
	portalDetailProxyTLS       = "proxy tls failed"
	portalDetailNotTheProxy    = "not the proxy"
	portalDetailTLSHandshake   = "tls handshake failed"
	portalDetailNoIssuerRoots  = "no proxied issuer roots"
	portalDetailNotComparable  = "host not comparable"
	portalRouteProxy           = "proxy"
	portalRouteDirect          = "direct"
	portalProxyDefaultHTTP     = "80"
	portalProxyDefaultHTTPS    = "443"
	portalProxyDefaultSOCKS    = "1080"
	portalProxyIssuerStartHTTP = 1 // the issuer's handshake is an http or SOCKS route's only TLSHandshakeStart
	portalProxyIssuerStartTLS  = 2 // behind an https proxy, net/http first reports the proxy hop's handshake
)

// portalProxyAttemptKey carries one address attempt's state to the proxy dial, which keeps the request's values
// but neither its cancellation nor its deadline.
type portalProxyAttemptKey struct{}

// errPortalShareEnded is the cause an attempt's context ends with when its share of the fetch's time runs out.
var errPortalShareEnded = errors.New("portal fetch: the attempt's share of the fetch's time ended")

// portalProxyAttempt is the state of one address attempt. Trace hooks can run on net/http's dial goroutine, after
// the attempt has returned, so every field below mu is read and written under it.
type portalProxyAttempt struct {
	deadline time.Time // the attempt's absolute deadline: its share, never later than the fetch's
	fetchEnd time.Time // the fetch's own deadline
	issuerAt int       // which TLSHandshakeStart is the issuer's

	mu        sync.Mutex
	timer     *time.Timer // ends the share; nil for the last attempt, whose share is all the time left
	conn      net.Conn    // the raw proxy socket
	dialed    bool        // the connection to the proxy, its TLS included, is up
	starts    int         // TLSHandshakeStart calls seen
	issuerTLS bool        // the issuer's handshake began within the share
	shareOut  bool        // the share ran out before the issuer's handshake began
	tlsFailed bool        // the issuer's handshake failed
}

// tlsStart stops the share and moves the socket to the fetch's deadline once the issuer's handshake begins,
// unless the share has already run out: whichever comes first under the mutex decides.
func (a *portalProxyAttempt) tlsStart() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.starts++
	if a.starts != a.issuerAt || a.shareOut {
		return
	}
	a.issuerTLS = true
	if a.timer != nil {
		a.timer.Stop()
	}
	if a.conn != nil {
		_ = a.conn.SetDeadline(a.fetchEnd)
	}
}

// endShare reports whether the share ran out before the issuer's handshake began, and records it.
func (a *portalProxyAttempt) endShare() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.issuerTLS {
		return false
	}
	a.shareOut = true
	return true
}

func (a *portalProxyAttempt) record(conn net.Conn) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.conn = conn
}

func (a *portalProxyAttempt) markDialed() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dialed = true
}

func (a *portalProxyAttempt) stopShare() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.timer != nil {
		a.timer.Stop()
	}
}

// portalProxyTransport is the single transport of one proxied verifier.
type portalProxyTransport struct {
	allowed   map[string]bool
	net       *portalNet
	proxyAddr string // the proxy's own host:port, the only address dialled
	proxyHost string
	socks     bool
	issuerAt  int
	base      *http.Transport

	tlsDoneHook func() // test seam: runs before a TLSHandshakeDone is recorded
}

// portalProxyAddr is the proxy's host and port, the scheme's default port when it names none.
func portalProxyAddr(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = portalProxyDefaultHTTPS
		case "socks5", "socks5h":
			port = portalProxyDefaultSOCKS
		default:
			port = portalProxyDefaultHTTP
		}
	}
	return net.JoinHostPort(u.Hostname(), port)
}

// newPortalProxyClient is the client of a verifier whose issuer host is listed: the direct client's bound and
// redirect refusal, over a transport that connects through the daemon proxy to the address it checked.
func newPortalProxyClient(pn *portalNet, allowed map[string]bool, issuerHost string) *http.Client {
	pp := &portalProxyTransport{allowed: allowed, net: pn, proxyAddr: portalProxyAddr(pn.proxy.URL), proxyHost: pn.proxy.URL.Hostname(),
		socks: pn.proxy.URL.Scheme == "socks5" || pn.proxy.URL.Scheme == "socks5h", issuerAt: portalProxyIssuerStartHTTP}
	if pn.proxy.URL.Scheme == "https" {
		pp.issuerAt = portalProxyIssuerStartTLS
	}
	proxyURL := pn.proxy.URL
	pp.base = &http.Transport{
		Proxy:             func(*http.Request) (*url.URL, error) { return proxyURL, nil },
		DisableKeepAlives: true,
		DialContext:       pp.dialProxy,
		DialTLSContext:    pp.dialProxyTLS,
		TLSClientConfig: &tls.Config{RootCAs: pn.proxy.IssuerRoots, ServerName: issuerHost, MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"}},
		OnProxyConnectResponse: func(_ context.Context, _ *url.URL, _ *http.Request, resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				return &portalFetchError{cause: portalCauseUnreachable, detail: fmt.Sprintf(portalDetailProxyStatus+"%dxx", resp.StatusCode/100)}
			}
			return nil
		},
		TLSHandshakeTimeout:    pn.timeout(),
		ResponseHeaderTimeout:  pn.timeout(),
		MaxResponseHeaderBytes: 64 << 10,
	}
	return &http.Client{
		Transport: pp,
		Timeout:   pn.timeout(),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return &portalFetchError{cause: portalCauseAddressRefused, detail: "redirect refused"}
		},
	}
}

// RoundTrip pins the authority, resolves the host and checks every answer before the proxy is contacted, then
// tries each checked address in the resolver's order, each in its own tunnel with its share of the time left.
func (pp *portalProxyTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" || !pp.allowed[portalAuthority(req.URL)] {
		return nil, &portalFetchError{cause: portalCauseAddressRefused, detail: "destination not allowed"}
	}
	ctx := req.Context()
	ips, err := pp.net.resolve(ctx, req.URL.Hostname())
	if err != nil || len(ips) == 0 {
		return nil, &portalFetchError{cause: portalCauseUnreachable, detail: "name not resolved"}
	}
	for _, ip := range ips {
		if portalAddressRefused(ip, false) {
			return nil, &portalFetchError{cause: portalCauseAddressRefused, detail: "address refused"}
		}
	}
	_, port, _ := net.SplitHostPort(portalAuthority(req.URL))
	fetchEnd, ok := ctx.Deadline()
	if !ok {
		fetchEnd = time.Now().Add(pp.net.timeout())
	}
	err = ctx.Err()
	for i := 0; i < len(ips) && ctx.Err() == nil; i++ {
		var resp *http.Response
		var a *portalProxyAttempt
		resp, a, err = pp.attempt(req, ips[i], port, fetchEnd, len(ips)-i)
		if err == nil {
			return resp, nil
		}
		if err = pp.mapAttempt(ctx, a, err); !portalProxyNextAddress(err) {
			return nil, err
		}
	}
	return nil, err
}

// attempt sends one copy of the request to one checked address. Its context carries the attempt's state for the
// dial and a trace of its own; a timer ends it at its share unless the issuer's handshake has begun.
func (pp *portalProxyTransport) attempt(req *http.Request, ip net.IP, port string, fetchEnd time.Time, left int) (*http.Response, *portalProxyAttempt, error) {
	share := time.Until(fetchEnd) / time.Duration(left)
	a := &portalProxyAttempt{deadline: time.Now().Add(share), fetchEnd: fetchEnd, issuerAt: pp.issuerAt}
	if left == 1 {
		a.deadline = fetchEnd
	}
	ctx, cancel := context.WithCancelCause(req.Context())
	if left > 1 {
		a.mu.Lock()
		a.timer = time.AfterFunc(share, func() {
			if a.endShare() {
				cancel(errPortalShareEnded)
			}
		})
		a.mu.Unlock()
	}
	trace := &httptrace.ClientTrace{
		TLSHandshakeStart: a.tlsStart,
		TLSHandshakeDone: func(_ tls.ConnectionState, err error) {
			if pp.tlsDoneHook != nil {
				pp.tlsDoneHook()
			}
			if err != nil {
				a.mu.Lock()
				a.tlsFailed = a.tlsFailed || a.starts >= a.issuerAt
				a.mu.Unlock()
			}
		},
	}
	ctx = httptrace.WithClientTrace(context.WithValue(ctx, portalProxyAttemptKey{}, a), trace)
	out := req.Clone(ctx)
	out.URL.Host = net.JoinHostPort(ip.String(), port)
	out.Host = req.Host
	if out.Host == "" {
		out.Host = req.URL.Host
	}
	resp, err := pp.base.RoundTrip(out)
	if err != nil {
		a.stopShare()
		cancel(nil)
		return nil, a, err
	}
	resp.Body = &portalProxyBody{ReadCloser: resp.Body, done: cancel}
	return resp, a, nil
}

// mapAttempt names a failed attempt: cancellation first, then a fetch error of ours, then the attempt's stage,
// then a failed issuer handshake, and otherwise a fixed word. No native error text is kept.
func (pp *portalProxyTransport) mapAttempt(ctx context.Context, a *portalProxyAttempt, err error) error {
	var fe *portalFetchError
	isOurs := errors.As(err, &fe)
	if ctx.Err() != nil && !isOurs {
		return ctx.Err()
	}
	if isOurs {
		return fe
	}
	a.mu.Lock()
	// The socket's deadline and the share's timer end together, and either can be seen first: an attempt that
	// reached its deadline before the issuer's handshake began ran out of its share.
	shareOut := a.shareOut || (!a.issuerTLS && !time.Now().Before(a.deadline))
	dialed, issuerTLS, tlsFailed := a.dialed, a.issuerTLS, a.tlsFailed
	a.mu.Unlock()
	switch {
	case shareOut && dialed:
		return &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyTarget}
	case shareOut:
		return &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyConnect}
	case pp.socks && dialed && !issuerTLS:
		// Go's SOCKS dialer returns a failed target connect, and a failed SOCKS login, as plain error text: the
		// label comes from the route and the stage, never from that text.
		return &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyTarget}
	case tlsFailed:
		return &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailTLSHandshake}
	}
	return &portalFetchError{cause: portalCauseUnreachable, detail: portalFetchDetail(err)}
}

// portalProxyNextAddress reports whether a failed attempt was a proxy-side failure before the issuer's TLS
// began, after which the next checked address is tried.
func portalProxyNextAddress(err error) bool {
	var fe *portalFetchError
	if !errors.As(err, &fe) {
		return false
	}
	return fe.detail == portalDetailProxyConnect || fe.detail == portalDetailProxyTarget || strings.HasPrefix(fe.detail, portalDetailProxyStatus)
}

// dialRaw dials only the proxy, under the attempt's absolute deadline, which stays on the socket: net/http
// detaches connection setup from the request, so only the socket's own deadline bounds the CONNECT exchange,
// SOCKS negotiation and the handshakes. It never uses the direct path's dial.
func (pp *portalProxyTransport) dialRaw(ctx context.Context, network, addr string) (net.Conn, *portalProxyAttempt, error) {
	a, _ := ctx.Value(portalProxyAttemptKey{}).(*portalProxyAttempt)
	if a == nil {
		return nil, nil, &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyConnect}
	}
	if !strings.EqualFold(addr, pp.proxyAddr) {
		return nil, nil, &portalFetchError{cause: portalCauseAddressRefused, detail: portalDetailNotTheProxy}
	}
	raw, err := (&net.Dialer{Deadline: a.deadline}).DialContext(ctx, network, addr)
	if err != nil {
		return nil, nil, &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyConnect}
	}
	if err := raw.SetDeadline(a.deadline); err != nil {
		_ = raw.Close()
		return nil, nil, &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyConnect}
	}
	a.record(raw)
	return raw, a, nil
}

// dialProxy is the dial of an http or SOCKS proxy.
func (pp *portalProxyTransport) dialProxy(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, a, err := pp.dialRaw(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	a.markDialed()
	return raw, nil
}

// dialProxyTLS is the dial of an https proxy: the same dial and deadline, then the proxy's own handshake under
// its own roots. It returns the *tls.Conn itself, so net/http reports the proxy hop's handshake and the issuer's
// is the second; a socket net/http never receives is closed here.
func (pp *portalProxyTransport) dialProxyTLS(ctx context.Context, network, addr string) (net.Conn, error) {
	raw, a, err := pp.dialRaw(ctx, network, addr)
	if err != nil {
		return nil, err
	}
	conn := tls.Client(raw, &tls.Config{ServerName: pp.proxyHost, RootCAs: pp.net.proxy.ProxyRoots, MinVersion: tls.VersionTLS12})
	if err := conn.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		if !time.Now().Before(a.deadline) {
			// The attempt's time ran out before the proxy finished its handshake: our connection to the proxy
			// never completed, and the next address is tried.
			return nil, &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyConnect}
		}
		return nil, &portalFetchError{cause: portalCauseUnreachable, detail: portalDetailProxyTLS}
	}
	a.markDialed()
	return conn, nil
}

// portalProxyBody ends the attempt's context once the response body is closed.
type portalProxyBody struct {
	io.ReadCloser
	done context.CancelCauseFunc
}

func (b *portalProxyBody) Close() error {
	err := b.ReadCloser.Close()
	b.done(nil)
	return err
}
