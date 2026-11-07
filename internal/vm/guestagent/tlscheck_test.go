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
	// A name that does not resolve: only the proxy can reach the server.
	code, out, errw := runTLSCheck(t, c, "127.0.0.1:"+portOf(srv))
	if code != 0 || connects.Load() != 1 || gotTarget.Load() != "127.0.0.1:"+portOf(srv) {
		t.Fatalf("code=%d out=%q err=%q connects=%d target=%v", code, out, errw, connects.Load(), gotTarget.Load())
	}
	// Without the pool the same path fails TLS, not the proxy.
	code, _, errw = runTLSCheck(t, tlsChecker{getenv: c.getenv}, "127.0.0.1:"+portOf(srv))
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
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()
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
