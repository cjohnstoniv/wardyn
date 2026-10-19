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
	"strings"
)

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
	var op *net.OpError
	if errors.As(err, &op) && op.Op == "proxyconnect" {
		// The proxy's address is the dial step's own: the operation wrapping it carries no address.
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
	hint := fmt.Sprintf("the server presented a certificate for %s issued by %s", distinguished(leaf.Subject), distinguished(leaf.Issuer))
	var unknown x509.UnknownAuthorityError
	if errors.As(cause, &unknown) {
		hint += "; if a proxy inspects TLS here, pass its root with --trusted-ca-file"
	}
	return hint
}

// distinguished is a certificate name as "CN (O)", quoted, with whichever parts it has.
func distinguished(n pkix.Name) string {
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
