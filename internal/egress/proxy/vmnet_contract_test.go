// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vmnet"
	"github.com/cjohnstoniv/wardyn/internal/vmnet/vmnettest"
)

// vmRun wires a guest to a real relay and the relay to this proxy, as a
// desktop VM run is wired: the guest has no proxy setting and reaches nothing
// but the relay.
func vmRun(t *testing.T, spec types.RunPolicySpec, upstream string) (*vmnettest.Guest, *syncBuf) {
	t.Helper()
	p, buf := newRuleTestProxy(t, spec, upstream, true, "", nil)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	rl, err := vmnet.New(vmnet.Config{Proxy: strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	return vmnettest.Attach(t, rl.Serve), buf
}

// waitRows waits for the decision log to hold n rows (the relay reports off
// the guest's path).
func waitRows(t *testing.T, buf *syncBuf, n int) []egress.DecisionLog {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if rs := rows(t, buf); len(rs) >= n {
			return rs
		}
	}
	t.Fatalf("decision log %q: want %d rows", buf.String(), n)
	return nil
}

// TestVMNetContract runs the relay against the real proxy: one row per guest
// flow, the TLS server-name binding decided before the proxy dials, and the
// port rule on.
func TestVMNetContract(t *testing.T) {
	var accepted atomic.Int32
	origin := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "origin")
	}))
	origin.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			accepted.Add(1)
		}
	}
	origin.StartTLS()
	defer origin.Close()
	g, buf := vmRun(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}}, strings.TrimPrefix(origin.URL, "https://"))
	addr := g.Resolve(t, "origin", "test")

	get := func(sni string) (string, error) {
		c, err := g.Dial(addr, 443, 3*time.Second)
		if err != nil {
			return "", err
		}
		defer c.Close()
		tc := tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		_ = tc.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.WriteString(tc, "GET / HTTP/1.1\r\nHost: "+sni+"\r\nConnection: close\r\n\r\n"); err != nil {
			return "", err
		}
		resp, err := http.ReadResponse(bufio.NewReader(tc), nil)
		if err != nil {
			return "", err
		}
		b, _ := io.ReadAll(resp.Body)
		return string(b), nil
	}

	t.Run("an allowed flow: one allow row, one dial", func(t *testing.T) {
		buf.Reset()
		if body, err := get("origin.test"); err != nil || body != "origin" {
			t.Fatalf("GET = %q, %v", body, err)
		}
		rs := waitRows(t, buf, 1)
		if len(rs) != 1 || rs[0].Decision != egress.Allow || rs[0].Flow == "" || accepted.Load() != 1 {
			t.Fatalf("rows %+v after %d dials", rs, accepted.Load())
		}
	})
	t.Run("SNI fronting: one deny row, no dial, no allow", func(t *testing.T) {
		buf.Reset()
		if _, err := get("www.other.test"); err == nil {
			t.Fatal("a fronted TLS connection got an answer")
		}
		waitRows(t, buf, 1)
		time.Sleep(100 * time.Millisecond) // nothing else may follow it
		rs := rows(t, buf)
		if len(rs) != 1 || rs[0].RuleSource != "vmnet:sni-mismatch" || rs[0].Flow == "" || rs[0].Request.Host != "origin.test" {
			t.Fatalf("rows %+v", rs)
		}
		if accepted.Load() != 1 {
			t.Fatalf("the proxy dialled for a fronted flow (%d dials)", accepted.Load())
		}
	})
	t.Run("a bare host on :22 is refused by the port rule", func(t *testing.T) {
		buf.Reset()
		if c, err := g.Dial(addr, 22, 3*time.Second); err == nil {
			c.Close()
			t.Fatal("connected to :22 of a bare allowlist host")
		}
		if rs := waitRows(t, buf, 1); rs[0].RuleSource != ruleSourcePort || rs[0].Flow == "" {
			t.Fatalf("rows %+v", rs)
		}
	})
	t.Run("a literal address makes a vmnet row", func(t *testing.T) {
		buf.Reset()
		if c, err := g.Dial([4]byte{93, 184, 216, 34}, 443, 3*time.Second); err == nil {
			c.Close()
			t.Fatal("connected to a literal address")
		}
		if rs := waitRows(t, buf, 1); rs[0].RuleSource != "vmnet:literal-ip" || rs[0].Request.Host != "93.184.216.34" {
			t.Fatalf("rows %+v", rs)
		}
	})
}

// TestVMNetPlainHTTPMethodRule: a VM's port-80 traffic reaches the proxy as
// forward requests, so allowed_methods applies to it as to a container's.
func TestVMNetPlainHTTPMethodRule(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path)
	}))
	defer origin.Close()
	g, buf := vmRun(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}, AllowedMethods: []string{http.MethodGet, http.MethodConnect}},
		strings.TrimPrefix(origin.URL, "http://"))
	addr := g.Resolve(t, "origin", "test")
	send := func(method string) *http.Response {
		c, err := g.Dial(addr, 80, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = c.Close() })
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		_, _ = io.WriteString(c, method+" /p HTTP/1.1\r\nHost: origin.test\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	if resp := send(http.MethodGet); resp.StatusCode != http.StatusOK {
		t.Fatalf("GET = %d", resp.StatusCode)
	}
	resp := send(http.MethodPost)
	if resp.StatusCode != http.StatusForbidden || resp.Header.Get(egressHeaderReason) != "policy:method" {
		t.Fatalf("POST = %d %v, want the proxy's policy:method refusal", resp.StatusCode, resp.Header)
	}
	rs := rows(t, buf)
	if len(rs) != 2 || rs[0].Request.Method != http.MethodGet || rs[0].Request.Path != "/p" || rs[1].RuleSource != "policy:method" {
		t.Fatalf("rows %+v: want the GET allowed and the POST refused, each with its method and path", rs)
	}
}
