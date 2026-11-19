// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
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
)

// bracketedBlockedAuthorities are loopback and metadata addresses in an IPv6 authority's brackets, with no
// port and with one.
var bracketedBlockedAuthorities = []string{
	"[::1]", "[169.254.169.254]", "[::ffff:169.254.169.254]", "[::1].",
	"[::1]:443", "[169.254.169.254]:443",
}

// A blocked address in brackets with no port is refused at step 0 like every literal in a blocked range,
// under every policy, so no review mode can raise an approval for it; before, the brackets stayed on and
// the host passed step 0 as a name net.ParseIP cannot read.
func TestBracketedLiteralIPIsRefusedAtStepZero(t *testing.T) {
	policies := map[string]types.RunPolicySpec{
		"allow_all_egress": {AllowAllEgress: true},
		"wait_for_review":  {AllowedDomains: []string{"example.com"}, FirstUseApproval: types.FirstUseWaitForReview},
		"deny_with_review": {AllowedDomains: []string{"example.com"}, FirstUseApproval: types.FirstUseDenyWithReview},
		"default-deny":     {AllowedDomains: []string{"example.com"}},
	}
	for name, spec := range policies {
		p, _ := newRuleTestProxy(t, spec, "127.0.0.1:1", false, "", nil)
		for _, a := range bracketedBlockedAuthorities {
			for _, method := range []string{http.MethodConnect, http.MethodGet} {
				host, port := splitHostPort(a, 443)
				d, _, log := p.evaluate(context.Background(), host, port, method, "")
				if d != egress.Deny || log == nil || log.RuleSource != "builtin:private-ip" {
					t.Errorf("%s: %s %s (host %q) = %s %v, want deny builtin:private-ip", name, method, a, host, d, log)
				}
			}
		}
	}
	allowAll, _ := newRuleTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, "127.0.0.1:1", false, "", nil)
	for _, a := range []string{"example.com", "example.com:443", "[2001:4860:4860::8888]"} {
		host, port := splitHostPort(a, 443)
		if _, _, log := allowAll.evaluate(context.Background(), host, port, http.MethodConnect, ""); log == nil || log.RuleSource != "policy:allowed" {
			t.Errorf("allow_all_egress: %s = %v, want policy:allowed", a, log)
		}
	}
}

// The guard itself reads a host still in brackets as the address it names, whatever hands it the host.
func TestLiteralIPGuardReadsABracketedHostAsItsAddress(t *testing.T) {
	p, _ := newRuleTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, "127.0.0.1:1", false, "", nil)
	for _, h := range []string{"[::1]", "[169.254.169.254]", "[::ffff:127.0.0.1]", "[fe80::1%25eth0]"} {
		if _, denied := p.literalIPGuard(egress.Request{Host: h, Port: 443}, h, 443); denied == nil || denied.RuleSource != "builtin:private-ip" {
			t.Errorf("literalIPGuard(%q) = %v, want deny builtin:private-ip", h, denied)
		}
	}
}

// A CONNECT to a bracketed blocked address with no port, and an absolute-form request whose authority is
// one, get the proxy's refusal and are never handed to a corporate upstream. net/http itself answers 400 to
// an IPv4 address in brackets, so that one need only never reach the corp proxy.
func TestBracketedLiteralIPNeverReachesTheCorpProxy(t *testing.T) {
	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(newUpstreamProxyPolicy(t, up, types.RunPolicySpec{AllowAllEgress: true}))
	defer srv.Close()
	refused := func(a, status string) bool {
		return strings.Contains(status, " 403 ") || a == "[169.254.169.254]" && strings.Contains(status, " 400 ")
	}
	for _, a := range []string{"[::1]", "[::ffff:169.254.169.254]", "[169.254.169.254]", "[::1]:443"} {
		conn, status := connectWithHost(t, srv.URL, a, a)
		_ = conn.Close()
		if !refused(a, status) {
			t.Errorf("CONNECT %s = %q, want a 403", a, status)
		}
		for _, hostHeader := range []string{a, "x.test"} {
			if status := rawProxyRequest(t, srv.URL, "GET http://"+a+"/ HTTP/1.1\r\nHost: "+hostHeader+"\r\n\r\n"); !refused(a, status) {
				t.Errorf("GET http://%s/ (Host %s) = %q, want a 403", a, hostHeader, status)
			}
		}
	}
	if n := atomic.LoadInt32(&f.accepts); n != 0 {
		got, _ := f.snapshot()
		t.Fatalf("the corp proxy was reached %d times (first %q)", n, got)
	}
}

// rawProxyRequest writes req to the proxy and returns the response's first bytes.
func rawProxyRequest(t *testing.T, proxyURL, req string) string {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	_, _ = io.WriteString(conn, req)
	buf := make([]byte, 4096)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	n, _ := conn.Read(buf)
	return string(buf[:n])
}
