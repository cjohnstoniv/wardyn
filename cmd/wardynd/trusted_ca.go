// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/cliutil"
	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// Every sandbox receives the bundle in one environment variable (WARDYN_MITM_CA_PEM, beside the
// run's own CA), and Linux refuses to start a process with one over 128 KiB. So boot warns above
// warnOrgTrustedCA and refuses above maxOrgTrustedCA, leaving room for the run's CA.
const (
	warnOrgTrustedCA   = 64 << 10
	maxOrgTrustedCA    = 120 << 10
	orgTrustedCARemedy = "every sandbox receives this bundle in one environment variable, which Linux caps at 128 KiB; name a file holding only your organisation's roots, not a whole system bundle"
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
	raw, rerr := readTrustedCAFile(path)
	if rerr != nil {
		return "", nil, 0, rerr
	}
	bundle, certs, perr := runner.TrustedCABundle(raw)
	if perr != nil {
		return "", nil, 0, fmt.Errorf("WARDYN_TRUSTED_CA_FILE %q: %w", path, perr)
	}
	if len(bundle) > maxOrgTrustedCA {
		return "", nil, 0, fmt.Errorf("WARDYN_TRUSTED_CA_FILE %q: %d bytes is over %d: %s", path, len(bundle), maxOrgTrustedCA, orgTrustedCARemedy)
	}
	sys, serr := x509.SystemCertPool()
	if serr != nil || sys == nil {
		sys = x509.NewCertPool() // no system pool on this platform: additive to an empty base
	}
	for _, c := range certs {
		sys.AddCert(c)
	}
	return bundle, sys, len(certs), nil
}

// readTrustedCAFile reads the bundle from the file it checked: a group- or world-writable bundle is
// refused, as for a secret file, because whoever can write it chooses the roots wardynd and every
// sandbox trust. Other-read is fine; a CA bundle is public.
func readTrustedCAFile(path string) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_TRUSTED_CA_FILE: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("WARDYN_TRUSTED_CA_FILE: %w", err)
	}
	if err := cliutil.CheckSecretFileMode("WARDYN_TRUSTED_CA_FILE", path, fi.Mode().Perm(), -1, os.Geteuid()); err != nil {
		return nil, err
	}
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("WARDYN_TRUSTED_CA_FILE: %w", err)
	}
	return raw, nil
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
	certs, err := runner.ParseCertsPEM([]byte(pemBundle))
	if err != nil {
		return nil
	}
	subjects := make([]string, 0, len(certs))
	for _, c := range certs {
		subjects = append(subjects, c.Subject.String())
	}
	return subjects
}

// logTrustedCA is the boot line for a configured trust bundle, out of run() (whose cyclomatic budget
// is full). Nothing is logged for an empty bundle.
func logTrustedCA(count int, pemBundle string) {
	if count > 0 {
		slog.Info("wardynd: corporate CA trust configured (WARDYN_TRUSTED_CA_FILE)",
			slog.Int("cert_count", count), slog.Any("subjects", certSubjects(pemBundle)))
	}
	if len(pemBundle) > warnOrgTrustedCA {
		slog.Warn("wardynd: WARDYN_TRUSTED_CA_FILE is over 64 KiB; "+orgTrustedCARemedy, slog.Int("bytes", len(pemBundle)))
	}
}
