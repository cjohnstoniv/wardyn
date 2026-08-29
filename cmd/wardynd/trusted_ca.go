// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"net/http"
	"os"
	"strings"
)

// loadTrustedCA reads WARDYN_TRUSTED_CA_FILE (path, empty = unset — the
// default) as a PEM bundle of additional roots a corporate TLS-inspecting
// middlebox signs with, and returns it alongside a cert pool seeded from the
// SYSTEM roots plus that bundle. "" is the fail-open, byte-identical-to-today
// case: (pem="", pool=nil, n=0, err=nil) — every outbound TLS client in this
// process trusts exactly the system roots, as it always has. A non-empty path
// that does not exist, or whose content contains no PEM-encoded certificate,
// is a boot error naming the var: this is an operator-typed trust boundary,
// so a typo must refuse to start rather than silently keep the old (narrower)
// trust set. n is the number of certificates the bundle actually added — used
// only for the boot log line; installTrustedCA needs just the pool.
//
// Deliberately NOT x509.CertPool.AppendCertsFromPEM (which reports only
// ok/not-ok): a per-certificate parse gives the boot log real subjects
// instead of "loaded a trusted CA file", the honesty the sibling
// WARDYN_AGENT_IMAGES/WARDYN_DEFAULT_POLICY boot lines already have.
func loadTrustedCA(path string) (pemBundle string, pool *x509.CertPool, n int, err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", nil, 0, nil
	}
	raw, rerr := os.ReadFile(path)
	if rerr != nil {
		return "", nil, 0, fmt.Errorf("WARDYN_TRUSTED_CA_FILE: %w", rerr)
	}
	certs, perr := parseCertsPEM(raw)
	if perr != nil {
		return "", nil, 0, fmt.Errorf("WARDYN_TRUSTED_CA_FILE %q: %w", path, perr)
	}
	sys, serr := x509.SystemCertPool()
	if serr != nil || sys == nil {
		sys = x509.NewCertPool() // no system pool on this platform: additive to an empty base
	}
	for _, c := range certs {
		sys.AddCert(c)
	}
	return string(raw), sys, len(certs), nil
}

// parseCertsPEM decodes every "CERTIFICATE" PEM block in raw. Any block that
// fails to parse as a certificate, or a raw input with no certificate block
// at all (garbage, or a key/CSR pasted by mistake), is an error — never a
// silent skip: a corp CA file that doesn't parse as configured is a
// configuration mistake, not a partial success.
func parseCertsPEM(raw []byte) ([]*x509.Certificate, error) {
	var certs []*x509.Certificate
	rest := raw
	for {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse certificate: %w", err)
		}
		certs = append(certs, cert)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("no PEM-encoded certificates found")
	}
	return certs, nil
}

// installTrustedCA additively sets pool as the RootCAs of tr's TLS config —
// mutating the existing *http.Transport (never replacing it) so every caller
// already holding a reference to tr (every package that dials via
// http.DefaultTransport) picks up the change with no wiring of their own.
// nil pool is a no-op: the WARDYN_TRUSTED_CA_FILE-unset case leaves tr
// untouched, byte-identical to today.
//
// Mutating in place also preserves whatever TLSClientConfig fields already
// exist (e.g. a pre-seeded NextProtos) instead of clobbering them with a
// fresh *tls.Config{RootCAs: pool} — and leaves ForceAttemptHTTP2 (already
// true on http.DefaultTransport) untouched, since that field lives on
// *http.Transport, not *tls.Config.
//
// ponytail: this mutates the ONE process-global http.DefaultTransport, so it
// is process-wide the instant it runs — fine for wardynd's own outbound
// calls (OIDC, GitHub App, audit webhook), all of which already share that
// transport and none of which need a per-call override; add a per-client
// override only if a future caller needs the corp CA NOT to apply.
func installTrustedCA(tr *http.Transport, pool *x509.CertPool) {
	if pool == nil {
		return
	}
	if tr.TLSClientConfig == nil {
		tr.TLSClientConfig = &tls.Config{}
	}
	tr.TLSClientConfig.RootCAs = pool
}

// certSubjects re-parses pemBundle purely to give the boot log human-readable
// subjects. loadTrustedCA already proved pemBundle parses (this is called only
// when it returned a positive count), so an error here should never happen;
// it degrades to nil (an empty subjects list) rather than a second boot
// failure over a log line.
func certSubjects(pemBundle string) []string {
	certs, err := parseCertsPEM([]byte(pemBundle))
	if err != nil {
		return nil
	}
	subjects := make([]string, 0, len(certs))
	for _, c := range certs {
		subjects = append(subjects, c.Subject.String())
	}
	return subjects
}
