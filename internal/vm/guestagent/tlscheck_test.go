// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/x509"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func tlsCheckServer(t *testing.T) (*httptest.Server, *x509.CertPool) {
	t.Helper()
	srv := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	t.Cleanup(srv.Close)
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	return srv, pool
}

func runTLSCheck(t *testing.T, c tlsChecker, args ...string) (int, string, string) {
	t.Helper()
	var out, errw bytes.Buffer
	code := c.run(context.Background(), args, &out, &errw)
	return code, out.String(), errw.String()
}

func TestTLSCheckUntrustedCertExits1(t *testing.T) {
	srv, _ := tlsCheckServer(t)
	code, out, errw := runTLSCheck(t, tlsChecker{}, srv.Listener.Addr().String())
	if code != 1 || out != "" || !strings.HasPrefix(errw, "TLS failed: 127.0.0.1:") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errw)
	}
}

func TestTLSCheckTrustedCertExits0(t *testing.T) {
	srv, pool := tlsCheckServer(t)
	code, out, errw := runTLSCheck(t, tlsChecker{roots: pool}, "https://"+srv.Listener.Addr().String())
	if code != 0 || errw != "" || !strings.HasPrefix(out, "trusted: 127.0.0.1:") || !strings.Contains(out, "issued by") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errw)
	}
}

func TestTLSCheckThroughProxy(t *testing.T) {
	srv, pool := tlsCheckServer(t)
	var connects atomic.Int32
	var gotTarget atomic.Value
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pl.Close() })
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				br := bufio.NewReader(c)
				req, err := http.ReadRequest(br)
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				connects.Add(1)
				gotTarget.Store(req.Host)
				up, err := net.Dial("tcp", srv.Listener.Addr().String())
				if err != nil {
					return
				}
				defer up.Close()
				_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				go func() { _, _ = io.Copy(up, br) }()
				_, _ = io.Copy(c, up)
			}()
		}
	}()
	env := map[string]string{"HTTPS_PROXY": "http://" + pl.Addr().String()}
	c := tlsChecker{roots: pool, getenv: func(k string) string { return env[k] }}
	// A name that does not resolve here (the test certificate names it): only the proxy can reach the server.
	code, out, errw := runTLSCheck(t, c, "example.com:"+portOf(srv))
	if code != 0 || connects.Load() != 1 || gotTarget.Load() != "example.com:"+portOf(srv) {
		t.Fatalf("code=%d out=%q err=%q connects=%d target=%v", code, out, errw, connects.Load(), gotTarget.Load())
	}
	// Without the pool the same path fails TLS, not the proxy.
	code, _, errw = runTLSCheck(t, tlsChecker{getenv: c.getenv}, "example.com:"+portOf(srv))
	if code != 1 || !strings.HasPrefix(errw, "TLS failed:") {
		t.Fatalf("code=%d err=%q", code, errw)
	}
}

func portOf(s *httptest.Server) string {
	_, p, _ := net.SplitHostPort(s.Listener.Addr().String())
	return p
}

func TestTLSCheckBadArgumentExits2(t *testing.T) {
	for _, args := range [][]string{nil, {""}, {"a", "b"}, {"http://example.com"}, {"example.com:0"}, {"example.com:99999"}, {"example.com:x"}, {"https://"}} {
		code, out, errw := runTLSCheck(t, tlsChecker{}, args...)
		if code != 2 || out != "" || errw == "" {
			t.Errorf("args %q: code=%d out=%q err=%q", args, code, out, errw)
		}
	}
}

func TestTLSCheckNetworkErrorExits2(t *testing.T) {
	addr := "127.0.0.1:1" // nothing listens there; a freed port could be taken by another process
	code, _, errw := runTLSCheck(t, tlsChecker{}, addr)
	if code != 2 || strings.HasPrefix(errw, "TLS failed") {
		t.Fatalf("code=%d err=%q", code, errw)
	}
}

func TestParseTLSCheckTarget(t *testing.T) {
	for in, want := range map[string]string{
		"example.com":              "example.com:443",
		"example.com:8443":         "example.com:8443",
		"https://example.com":      "example.com:443",
		"https://example.com/a/b":  "example.com:443",
		"https://example.com:8443": "example.com:8443",
		"[::1]:8443":               "[::1]:8443",
	} {
		got, err := parseTLSCheckTarget(in)
		if err != nil || got != want {
			t.Errorf("%q: got %q, %v; want %q", in, got, err, want)
		}
	}
}

// proxyListener accepts connections for a test, hands each to serve, and counts them.
func proxyListener(t *testing.T, serve func(net.Conn)) (addr string, conns *atomic.Int32) {
	t.Helper()
	pl, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pl.Close() })
	conns = new(atomic.Int32)
	go func() {
		for {
			c, err := pl.Accept()
			if err != nil {
				return
			}
			conns.Add(1)
			go func() {
				defer c.Close()
				serve(c)
			}()
		}
	}()
	return pl.Addr().String(), conns
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// The run proxy is plain http: an https:// proxy URL is refused before any byte is sent, so the
// CONNECT (and any credentials in the URL) never travel in the clear to a TLS port.
func TestTLSCheckRefusesHTTPSProxyScheme(t *testing.T) {
	var bytesIn atomic.Int32
	addr, conns := proxyListener(t, func(c net.Conn) {
		buf := make([]byte, 512)
		n, _ := c.Read(buf)
		bytesIn.Add(int32(n))
	})
	env := map[string]string{"HTTPS_PROXY": "https://tester:invented-pass@" + addr}
	code, out, errw := runTLSCheck(t, tlsChecker{getenv: envOf(env)}, "example.com:443")
	if code != 2 || out != "" || !strings.Contains(errw, "http") || !strings.Contains(errw, "https") {
		t.Fatalf("code=%d out=%q err=%q", code, out, errw)
	}
	if conns.Load() != 0 || bytesIn.Load() != 0 {
		t.Fatalf("the https proxy URL was dialed: conns=%d bytes=%d", conns.Load(), bytesIn.Load())
	}
}

// URL credentials go in Proxy-Authorization on the CONNECT, never in Authorization.
func TestTLSCheckProxyCredentialsUseProxyAuthorization(t *testing.T) {
	srv, pool := tlsCheckServer(t)
	const want = "Basic dGVzdGVyOmludmVudGVkLXBhc3M=" // tester:invented-pass
	var gotProxyAuth, gotAuth atomic.Value
	addr, _ := proxyListener(t, func(c net.Conn) {
		br := bufio.NewReader(c)
		req, err := http.ReadRequest(br)
		if err != nil || req.Method != http.MethodConnect {
			return
		}
		gotProxyAuth.Store(req.Header.Get("Proxy-Authorization"))
		gotAuth.Store(req.Header.Get("Authorization"))
		if req.Header.Get("Proxy-Authorization") != want {
			_, _ = io.WriteString(c, "HTTP/1.1 407 Proxy Authentication Required\r\nProxy-Authenticate: Basic\r\n\r\n")
			return
		}
		up, err := net.Dial("tcp", srv.Listener.Addr().String())
		if err != nil {
			return
		}
		defer up.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\n")
		go func() { _, _ = io.Copy(up, br) }()
		_, _ = io.Copy(c, up)
	})
	env := map[string]string{"HTTPS_PROXY": "http://tester:invented-pass@" + addr}
	code, out, errw := runTLSCheck(t, tlsChecker{roots: pool, getenv: envOf(env)}, "example.com:"+portOf(srv))
	if code != 0 {
		t.Fatalf("code=%d out=%q err=%q proxy-auth=%v", code, out, errw, gotProxyAuth.Load())
	}
	if gotProxyAuth.Load() != want || gotAuth.Load() != "" {
		t.Fatalf("Proxy-Authorization=%v Authorization=%v", gotProxyAuth.Load(), gotAuth.Load())
	}
}

// A proxy URL without a port is dialed on :80, as net/http does for an http proxy.
func TestTLSCheckProxyWithoutPortDefaultsTo80(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	env := map[string]string{"HTTPS_PROXY": "http://127.0.0.1"}
	_, err := dialTLSCheck(ctx, envOf(env), "example.com:443")
	if err == nil || strings.Contains(err.Error(), "missing port") || !strings.Contains(err.Error(), "127.0.0.1:80") {
		t.Fatalf("err = %v, want a dial of 127.0.0.1:80", err)
	}
}

// A proxy that sends bytes right behind its 200 is refused and the connection is closed.
func TestTLSCheckProxyEarlyDataClosesConn(t *testing.T) {
	closed := make(chan error, 1)
	addr, _ := proxyListener(t, func(c net.Conn) {
		if _, err := http.ReadRequest(bufio.NewReader(c)); err != nil {
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection established\r\n\r\nEARLY")
		_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
		_, err := io.Copy(io.Discard, c)
		closed <- err
	})
	env := map[string]string{"HTTPS_PROXY": "http://" + addr}
	code, _, errw := runTLSCheck(t, tlsChecker{getenv: envOf(env)}, "example.com:443")
	if code != 2 || !strings.Contains(errw, "before the handshake") {
		t.Fatalf("code=%d err=%q", code, errw)
	}
	if err := <-closed; err != nil {
		t.Fatalf("the proxy connection was left open: %v", err)
	}
}
