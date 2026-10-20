// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/certbundle"
)

// orgClient is the one HTTP client builder in this command: every dial the
// CLI makes goes through it (TestOrgClient_IsTheOnlyClientBuilder fails on any
// other http.Client or http.Transport). It never follows a redirect, takes the
// proxy from the environment, and trusts the system roots plus the anchors
// `wardyn ca add` stored for origin, for origin's host only. An origin with no
// stored anchors gets the default transport, as before. timeout 0 is none.
func orgClient(origin string, timeout time.Duration) *http.Client {
	return orgClientTrusting(origin, timeout, nil)
}

// orgClientTrusting is orgClient with extra anchors beside the stored ones:
// `wardyn ca add` reads /healthz through the anchors it is about to store.
func orgClientTrusting(origin string, timeout time.Duration, extra []*x509.Certificate) *http.Client {
	c := &http.Client{Timeout: timeout, CheckRedirect: noRedirect}
	stored, err := storedAnchors(origin)
	if err != nil {
		c.Transport = refusedTransport{err}
		return c
	}
	anchors := append(stored, extra...)
	if len(anchors) == 0 {
		return c
	}
	u, err := url.Parse(origin)
	if err != nil {
		c.Transport = refusedTransport{err}
		return c
	}
	host := u.Hostname()
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.TLSClientConfig = &tls.Config{
		RootCAs: certbundle.Pool(anchors),
		// A certificate that does not name the organisation's host (an
		// https:// proxy's, say) is held to the system roots alone, so an
		// anchor never vouches for another host on this transport.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate presented")
			}
			if cs.PeerCertificates[0].VerifyHostname(host) == nil {
				return nil
			}
			inter := x509.NewCertPool()
			for _, ic := range cs.PeerCertificates[1:] {
				inter.AddCert(ic)
			}
			_, err := cs.PeerCertificates[0].Verify(x509.VerifyOptions{DNSName: cs.ServerName, Intermediates: inter})
			return err
		},
	}
	c.Transport = t
	return c
}

// refusedTransport fails every request with the reason the anchors were
// refused, so a bad store fails closed instead of falling back.
type refusedTransport struct{ err error }

func (r refusedTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.err }

// anchorRefusedError is a stored anchor file refused at use.
type anchorRefusedError struct {
	origin string
	err    error
}

func (e *anchorRefusedError) Error() string { return e.err.Error() }
func (e *anchorRefusedError) Unwrap() error { return e.err }

// anchorDir is where `wardyn ca` keeps one PEM file per organisation.
func anchorDir() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "wardyn", "ca"), nil
}

// anchorFile is the store file for raw's origin: <dir>/<host>_<port>.pem.
func anchorFile(raw string) (path, origin string, err error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Hostname() == "" {
		return "", "", fmt.Errorf("%q is not an http(s) URL with a host", raw)
	}
	port := u.Port()
	if port == "" {
		port = map[string]string{"https": "443", "http": "80"}[u.Scheme]
	}
	host := strings.ToLower(u.Hostname())
	dir, err := anchorDir()
	if err != nil {
		return "", "", err
	}
	name := strings.ReplaceAll(host, ":", "_") + "_" + port + ".pem"
	return filepath.Join(dir, name), u.Scheme + "://" + u.Host, nil
}

// storedAnchors is the anchors stored for origin, nil when there are none.
// A file that fails certbundle.CheckFile or does not parse is an error.
func storedAnchors(origin string) ([]*x509.Certificate, error) {
	path, _, err := anchorFile(origin)
	if err != nil {
		// No http(s) origin (or no config dir): nothing can be stored for it.
		return nil, nil
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err := certbundle.CheckFile(path); err != nil {
		return nil, &anchorRefusedError{origin, err}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, &anchorRefusedError{origin, err}
	}
	entries, err := certbundle.Parse(raw, time.Now())
	if err != nil {
		return nil, &anchorRefusedError{origin, fmt.Errorf("refusing the CA file %s: %w", path, err)}
	}
	out := make([]*x509.Certificate, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Cert)
	}
	return out, nil
}
