// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/http/httpproxy"
)

// TLSCheckVerb is the first argument that selects the TLS check: `wardyn-guest-agent tls-check
// <https URL or host[:port]>`. On macOS, Go verifies through the system trust store and the keychain's
// trust settings, so the check answers the way Safari and other system-trust apps do, not the way a
// tool that reads a CA variable does. It reads the run's HTTPS_PROXY as those apps do, makes the TLS
// handshake and sends no request. It runs as the caller and needs no privilege.
const TLSCheckVerb = "tls-check"

const (
	tlsCheckDefaultPort = "443"
	tlsCheckTimeout     = 20 * time.Second
)

// tlsChecker holds the seams a test needs: a root pool in place of the system trust store, and the
// environment lookup.
type tlsChecker struct {
	roots  *x509.CertPool
	getenv func(string) string
}

// TLSCheckMain runs the check for the agent's main, given the arguments after "tls-check", and returns
// the exit code: 0 when the host is trusted, 1 when TLS fails, 2 for a usage or network error.
func TLSCheckMain(ctx context.Context, args []string, out, errw io.Writer) int {
	return tlsChecker{}.run(ctx, args, out, errw)
}

func (c tlsChecker) run(ctx context.Context, args []string, out, errw io.Writer) int {
	if len(args) != 1 {
		_, _ = fmt.Fprintln(errw, "usage: wardyn-guest-agent tls-check <https URL or host[:port]>")
		return 2
	}
	target, err := parseTLSCheckTarget(args[0])
	if err != nil {
		_, _ = fmt.Fprintln(errw, "tls-check:", err)
		return 2
	}
	host, _, _ := net.SplitHostPort(target)
	getenv := c.getenv
	if getenv == nil {
		getenv = os.Getenv
	}
	ctx, cancel := context.WithTimeout(ctx, tlsCheckTimeout)
	defer cancel()
	conn, err := dialTLSCheck(ctx, getenv, target)
	if err != nil {
		_, _ = fmt.Fprintf(errw, "tls-check: %s: %v\n", target, err)
		return 2
	}
	defer conn.Close()
	tc := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: c.roots, MinVersion: tls.VersionTLS12})
	if err := tc.HandshakeContext(ctx); err != nil {
		if isTLSVerdict(err) {
			_, _ = fmt.Fprintf(errw, "TLS failed: %s: %v\n", target, err)
			return 1
		}
		_, _ = fmt.Fprintf(errw, "tls-check: %s: %v\n", target, err)
		return 2
	}
	leaf := tc.ConnectionState().PeerCertificates[0]
	_, _ = fmt.Fprintf(out, "trusted: %s (%s, issued by %s)\n", target, leaf.Subject.String(), leaf.Issuer.String())
	return 0
}

// isTLSVerdict reports whether a handshake error is the server's certificate or a TLS alert, as
// opposed to a connection that broke or timed out.
func isTLSVerdict(err error) bool {
	var (
		verify *tls.CertificateVerificationError
		alert  tls.AlertError
		record tls.RecordHeaderError
		unk    x509.UnknownAuthorityError
		inv    x509.CertificateInvalidError
		host   x509.HostnameError
	)
	return errors.As(err, &verify) || errors.As(err, &alert) || errors.As(err, &record) ||
		errors.As(err, &unk) || errors.As(err, &inv) || errors.As(err, &host)
}

// parseTLSCheckTarget reads "https://host[:port][/path]" or "host[:port]" and returns "host:port",
// with port 443 when none is given.
func parseTLSCheckTarget(arg string) (string, error) {
	if arg == "" {
		return "", errors.New("needs a host")
	}
	hostport := arg
	if strings.Contains(arg, "://") {
		u, err := url.Parse(arg)
		if err != nil || u.Scheme != "https" || u.Host == "" {
			return "", fmt.Errorf("%q is not an https URL", arg)
		}
		hostport = u.Host
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = strings.Trim(hostport, "[]"), tlsCheckDefaultPort
		if host == "" || strings.ContainsAny(host, "/ :") && net.ParseIP(host) == nil {
			return "", fmt.Errorf("%q is not a host or host:port", arg)
		}
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return "", fmt.Errorf("port %q is outside 1-65535", port)
	}
	if host == "" {
		return "", fmt.Errorf("%q has no host", arg)
	}
	return net.JoinHostPort(host, port), nil
}

// dialTLSCheck connects to target, through the run's HTTPS proxy (a CONNECT) when the environment
// names one for it, as system-trust apps do.
func dialTLSCheck(ctx context.Context, getenv func(string) string, target string) (net.Conn, error) {
	pick := func(upper string) string {
		if v := getenv(upper); v != "" {
			return v
		}
		return getenv(strings.ToLower(upper))
	}
	cfg := httpproxy.Config{HTTPSProxy: pick("HTTPS_PROXY"), NoProxy: pick("NO_PROXY")}
	proxy, err := cfg.ProxyFunc()(&url.URL{Scheme: "https", Host: target})
	if err != nil {
		return nil, fmt.Errorf("proxy setting: %w", err)
	}
	var d net.Dialer
	if proxy == nil {
		return d.DialContext(ctx, "tcp", target)
	}
	// The run proxy speaks plain http. Refuse any other scheme before a byte is written, so a CONNECT
	// and the URL's credentials never go in the clear to a port that expects TLS.
	if proxy.Scheme != "http" {
		return nil, fmt.Errorf("proxy %s: the run proxy is http, so a %s:// proxy URL is not supported", proxy.Host, proxy.Scheme)
	}
	proxyAddr := proxy.Host
	if proxy.Port() == "" {
		proxyAddr = net.JoinHostPort(proxy.Hostname(), "80")
	}
	conn, err := d.DialContext(ctx, "tcp", proxyAddr)
	if err != nil {
		return nil, fmt.Errorf("proxy %s: %w", proxyAddr, err)
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	req := &http.Request{Method: http.MethodConnect, URL: &url.URL{Opaque: target}, Host: target, Header: http.Header{}}
	if u := proxy.User; u != nil {
		pw, _ := u.Password()
		req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(u.Username()+":"+pw)))
	}
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy %s: %w", proxyAddr, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy %s: %w", proxyAddr, err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		_ = conn.Close()
		return nil, fmt.Errorf("proxy %s refused the tunnel: %s", proxyAddr, resp.Status)
	}
	_ = conn.SetDeadline(time.Time{})
	if br.Buffered() > 0 {
		_ = conn.Close()
		return nil, errors.New("proxy sent data before the handshake")
	}
	return conn, nil
}
