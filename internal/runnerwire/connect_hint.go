// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ConnectStage is the `wardyn-runnerd doctor` stage a failed connection or session belongs to, for the
// offline summary: identity for a refused proof, proxy, dns and tls by the error's own type, tls for any
// other dial failure (where doctor reports a connection that never opened), healthz for the rest (an
// address that answered something other than the organisation's stream, a session that dropped).
func ConnectStage(err error) string {
	var verify *tls.CertificateVerificationError
	var unknown x509.UnknownAuthorityError
	var host x509.HostnameError
	var invalid x509.CertificateInvalidError
	var record tls.RecordHeaderError
	var refused *proxyRefusedError
	var dns *net.DNSError
	var op *net.OpError
	switch {
	case errors.Is(err, ErrAuth) || errors.Is(err, ErrRevoked):
		return types.RunnerStageIdentity
	case errors.As(err, &refused), errors.As(err, &op) && op.Op == "proxyconnect":
		return types.RunnerStageProxy
	case errors.As(err, &verify), errors.As(err, &unknown), errors.As(err, &host), errors.As(err, &invalid), errors.As(err, &record):
		return types.RunnerStageTLS
	case errors.As(err, &dns):
		return types.RunnerStageDNS
	case errors.As(err, &op) && op.Op == "dial":
		return types.RunnerStageTLS
	}
	return types.RunnerStageHealthz
}

// connectHint says what a failed dial most likely means and what to do, or "" when the error is not one
// it recognises. It names hosts and certificate subjects only: a proxy URL may hold a credential, so it
// is never read from the error's text.
func connectHint(err error) string {
	var verify *tls.CertificateVerificationError
	if errors.As(err, &verify) && len(verify.UnverifiedCertificates) > 0 {
		return certificateHint(verify.UnverifiedCertificates[0], verify.Err)
	}
	var unknown x509.UnknownAuthorityError
	if errors.As(err, &unknown) && unknown.Cert != nil {
		return certificateHint(unknown.Cert, unknown)
	}
	var host x509.HostnameError
	if errors.As(err, &host) && host.Certificate != nil {
		return certificateHint(host.Certificate, host)
	}
	var refused *proxyRefusedError
	if errors.As(err, &refused) {
		return refused.hint()
	}
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "proxyconnect" {
		// The proxy's address is the dial step's own: the operation wrapping it carries no address.
		var dns *net.DNSError
		if errors.As(op.Err, &dns) && dns.Name != "" {
			return fmt.Sprintf("the proxy %s named by HTTPS_PROXY cannot be resolved", dns.Name)
		}
		var dial *net.OpError
		if errors.As(op.Err, &dial) && dial.Addr != nil {
			return fmt.Sprintf("the proxy %s named by HTTPS_PROXY did not accept the connection", dial.Addr)
		}
		return "the proxy named by HTTPS_PROXY did not accept the connection"
	}
	var dns *net.DNSError
	if errors.As(err, &dns) && dns.Name != "" {
		return "cannot resolve " + dns.Name
	}
	return ""
}

// certificateHint names the leaf the server presented and who issued it, which tells a proxy that
// inspects TLS (its own issuer) from a wrong host or an expired certificate.
func certificateHint(leaf *x509.Certificate, cause error) string {
	hint := fmt.Sprintf("the server presented a certificate for %s issued by %s", Distinguished(leaf.Subject), Distinguished(leaf.Issuer))
	var unknown x509.UnknownAuthorityError
	if errors.As(cause, &unknown) {
		hint += "; if a proxy inspects TLS here, pass its root with --trusted-ca-file"
	}
	return hint
}

// Distinguished is a certificate name as "CN (O)", quoted, with whichever parts it has.
func Distinguished(n pkix.Name) string {
	parts := []string{}
	if n.CommonName != "" {
		parts = append(parts, fmt.Sprintf("%q", n.CommonName))
	}
	if len(n.Organization) > 0 {
		parts = append(parts, fmt.Sprintf("(%s)", strings.Join(n.Organization, ", ")))
	}
	if len(parts) == 0 {
		return "an unnamed subject"
	}
	return strings.Join(parts, " ")
}

// proxyRefusedError is a proxy that answered the CONNECT request with 407 or 403. net/http reports any
// non-200 CONNECT answer as a plain error holding only the status text, so the dial classifies it once
// and carries the status and the proxy's host and port beside the original error.
type proxyRefusedError struct {
	err    error
	status int
	proxy  string
}

func (e *proxyRefusedError) Error() string { return e.err.Error() }
func (e *proxyRefusedError) Unwrap() error { return e.err }

func (e *proxyRefusedError) hint() string {
	why := "it does not allow this destination"
	if e.status == http.StatusProxyAuthRequired {
		why = "it wants proxy authentication"
	}
	proxy := "the proxy"
	if e.proxy != "" {
		proxy += " at " + e.proxy
	}
	return fmt.Sprintf("%s refused the connection (%d: %s)", proxy, e.status, why)
}

// classifyProxyRefusal wraps err as a proxyRefusedError when a proxy is in use and err's innermost error
// is exactly the standard status text of a 407 or 403. Only that comparison reads the text, which is the
// proxy's own reason phrase and is never printed; the proxy's host and port come from the transport's
// proxy function.
func classifyProxyRefusal(err error, address string, rt http.RoundTripper) error {
	leaf := err
	for next := errors.Unwrap(leaf); next != nil; next = errors.Unwrap(leaf) {
		leaf = next
	}
	status := 0
	for _, code := range []int{http.StatusProxyAuthRequired, http.StatusForbidden} {
		if leaf.Error() == http.StatusText(code) {
			status = code
		}
	}
	if status == 0 {
		return err
	}
	if rt == nil {
		rt = http.DefaultTransport
	}
	tr, ok := rt.(*http.Transport)
	if !ok || tr.Proxy == nil {
		return err
	}
	proxy, _ := orgProxy(address, tr.Proxy)
	if proxy == "" {
		return err
	}
	return &proxyRefusedError{err: err, status: status, proxy: proxy}
}

// OrgRoute is how the runner's own session to the organisation will leave this computer, for the one
// line serve logs at startup: "direct", or "via proxy host:port (HTTPS_PROXY)". It reads the same
// environment the dial's transport does and prints the proxy's host and port only.
func OrgRoute(orgURL, runnerID string) string {
	return orgRoute(orgURL, runnerID, http.ProxyFromEnvironment)
}

func orgRoute(orgURL, runnerID string, proxyFor func(*http.Request) (*url.URL, error)) string {
	address, err := StreamURL(orgURL, runnerID)
	if err != nil {
		return "direct"
	}
	host, variable := orgProxy(address, proxyFor)
	if host == "" {
		return "direct"
	}
	return fmt.Sprintf("via proxy %s (%s)", host, variable)
}

// orgProxy is the host and port of the proxy proxyFor names for the stream address, "" for none, and the
// variable that names it. Never the proxy's userinfo.
func orgProxy(address string, proxyFor func(*http.Request) (*url.URL, error)) (host, variable string) {
	u, err := url.Parse(address)
	if err != nil {
		return "", ""
	}
	variable = "HTTPS_PROXY"
	if u.Scheme == "ws" {
		u.Scheme, variable = "http", "HTTP_PROXY"
	} else {
		u.Scheme = "https"
	}
	proxy, err := proxyFor(&http.Request{URL: u})
	if err != nil || proxy == nil {
		return "", ""
	}
	return proxy.Host, variable
}
