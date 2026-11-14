// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
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

// orgTransport is the transport c, an orgClient, dials through, for a RoundTripper that wraps it.
func orgTransport(c *http.Client) http.RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}
	return http.DefaultTransport
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
		// Only a connection to the organisation's host goes through the
		// anchors. Any other (an https:// proxy's, say) is held to the system
		// roots alone, whatever names its certificate carries.
		VerifyConnection: func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("no certificate presented")
			}
			// The proxy t dials on its way to origin, the one host its
			// requests go to.
			var proxy *url.URL
			var proxyErr error
			if t.Proxy != nil {
				proxy, proxyErr = t.Proxy(&http.Request{URL: u})
			}
			if proxyErr == nil && toOrgHost(cs, host, proxy) {
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

// toOrgHost reports whether cs is a handshake with host, the organisation's,
// on a transport that dials proxy (nil for none) on its way there. It decides
// by the host dialled, never by the names the certificate carries:
// cs.ServerName is that host without its trailing dot, compared without case,
// or "" when an IP address was dialled.
func toOrgHost(cs tls.ConnectionState, host string, proxy *url.URL) bool {
	host = strings.TrimRight(host, ".")
	if cs.ServerName != "" {
		return strings.EqualFold(cs.ServerName, host)
	}
	// The handshake has already checked that the certificate names the
	// address dialled, so a certificate naming host's address is host's,
	// unless it also names the host of an https:// proxy at another address,
	// which then may be the one dialled.
	ip, leaf := net.ParseIP(host), cs.PeerCertificates[0]
	if ip == nil || leaf.VerifyHostname(host) != nil {
		return false
	}
	if proxy == nil || proxy.Scheme != "https" {
		return true
	}
	// net/http dials a proxy host that is not ASCII by its IDNA form, which
	// may be an address, so only an ASCII host is the name that was checked.
	ph := proxy.Hostname()
	ascii := strings.IndexFunc(ph, func(r rune) bool { return r >= 0x80 }) < 0
	return net.ParseIP(ph).Equal(ip) || ascii && leaf.VerifyHostname(ph) != nil
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

// storedAnchors is the usable anchors stored for origin, nil when there are
// none. certbundle.LoadFile checks the file on the descriptor it reads; a
// skipped entry is warned about once per process, and a file it refuses is an
// error.
func storedAnchors(origin string) ([]*x509.Certificate, error) {
	path, _, err := anchorFile(origin)
	if err != nil {
		// No http(s) origin (or no config dir): nothing can be stored for it.
		return nil, nil
	}
	if _, err := os.Lstat(path); errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	certs, warnings, err := certbundle.LoadFile(path, time.Now())
	for _, w := range warnings {
		if _, seen := anchorWarned.LoadOrStore(w, true); !seen {
			fmt.Fprintln(anchorWarnings, "wardyn: warning:", w)
		}
	}
	if err != nil {
		return nil, &anchorRefusedError{origin, err}
	}
	return certs, nil
}

// anchorWarned is the skipped-entry warnings already printed; anchorWarnings
// is where they go (a var so a test can read them).
var (
	anchorWarned   sync.Map
	anchorWarnings io.Writer = os.Stderr
)
