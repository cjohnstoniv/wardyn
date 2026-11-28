// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// rpProxy serves a Proxy on a loopback listener, built as newTestProxy builds one: every name resolves to a
// public address and every dial goes to upstream. Decisions are mirrored to io.Discard, so each one's encode and
// mask stay in the measured path; the queue to the control plane is one slot and drops the rest, as a full one does.
func rpProxy(b *testing.B, spec types.RunPolicySpec, upstream string, ap *approvalClient) *url.URL {
	b.Helper()
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(spec),
		Approval: ap,
		Sink:     &decisionSink{out: io.Discard, ch: make(chan queuedDecision, 1)},
		Resolver: publicResolver{},
		Dial:     redirectDial(upstream),
	})
	srv := httptest.NewServer(p)
	b.Cleanup(srv.Close)
	u, err := url.Parse(srv.URL)
	if err != nil {
		b.Fatal(err)
	}
	return u
}

// rpVia is a keep-alive client that sends every request through the proxy at u.
func rpVia(u *url.URL) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(u)}, Timeout: 10 * time.Second}
}

// rpGet is one timed GET of target, which must answer want.
func rpGet(b *testing.B, client *http.Client, target string, want int) {
	resp, err := client.Get(target)
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != want {
		b.Fatalf("GET %s = %d, want %d", target, resp.StatusCode, want)
	}
}

// rpGets runs b.N GETs of target.
func rpGets(b *testing.B, client *http.Client, target string, want int) {
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rpGet(b, client, target, want)
	}
}

// rpEcho is a loopback TCP echo server, the stand-in for an opaque TLS upstream.
func rpEcho(b *testing.B) string {
	b.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				_, _ = io.Copy(c, c)
				_ = c.Close()
			}()
		}
	}()
	return ln.Addr().String()
}

// rpTunnel opens one CONNECT tunnel to allowed.test through the proxy at addr and sends 4 bytes round it.
func rpTunnel(b *testing.B, addr string) {
	c, err := net.Dial("tcp", addr)
	if err != nil {
		b.Fatal(err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(c, "CONNECT allowed.test:443 HTTP/1.1\r\nHost: allowed.test:443\r\n\r\n"); err != nil {
		b.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		b.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		b.Fatalf("CONNECT = %d, want 200", resp.StatusCode)
	}
	if _, err := io.WriteString(c, "ping"); err != nil {
		b.Fatal(err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(br, got); err != nil || string(got) != "ping" {
		b.Fatalf("tunnel echo %q: %v", got, err)
	}
}

// BenchmarkProxyRequestPath is one request through the egress proxy on loopback, beside the same request made
// with no proxy: direct (no proxy), allow (plain HTTP to an allowed host), deny (refused by policy), hold (a
// wait_for_review host whose approval is already decided, the path every request after a host's first takes) and
// connect (a new CONNECT tunnel and one 4-byte round trip through it). Compare runs with benchstat.
func BenchmarkProxyRequestPath(b *testing.B) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	}))
	b.Cleanup(upstream.Close)
	spec := types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}

	b.Run("direct", func(b *testing.B) {
		rpGets(b, &http.Client{Timeout: 10 * time.Second}, upstream.URL+"/", http.StatusOK)
	})
	b.Run("allow", func(b *testing.B) {
		rpGets(b, rpVia(rpProxy(b, spec, upstreamAddr(upstream), nil)), "http://allowed.test/", http.StatusOK)
	})
	b.Run("deny", func(b *testing.B) {
		rpGets(b, rpVia(rpProxy(b, spec, upstreamAddr(upstream), nil)), "http://denied.test/", http.StatusForbidden)
	})
	b.Run("hold", func(b *testing.B) {
		saved := holdPollInterval
		holdPollInterval = 5 * time.Millisecond
		b.Cleanup(func() { holdPollInterval = saved })
		cp := approvalCPStub(apState(types.ApprovalApproved), nil, nil)
		b.Cleanup(cp.Close)
		ap := newApprovalClient(cp.URL, newTokenSource("tok"), uuid.New(), cp.Client())
		ap.configureHold(types.FirstUseWaitForReview, 30*time.Second, 16)
		held := spec
		held.FirstUseApproval = types.FirstUseWaitForReview
		client := rpVia(rpProxy(b, held, upstreamAddr(upstream), ap))
		rpGet(b, client, "http://held.test/", http.StatusOK) // raises the approval and waits for it: not timed
		rpGets(b, client, "http://held.test/", http.StatusOK)
	})
	b.Run("connect", func(b *testing.B) {
		addr := rpProxy(b, spec, rpEcho(b), nil).Host
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			rpTunnel(b, addr)
		}
	})
}
