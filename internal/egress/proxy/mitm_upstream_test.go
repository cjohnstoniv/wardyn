// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

// The operator-reported 0.7.8 defect: per-user AWS SSO is dead on an estate
// that reaches AWS through a corporate proxy, diagnosed as "the MITM lane does
// not honour SiteConfig.upstream_proxy_url". Reading the code says otherwise —
// serveMITMRequest resolves through egressTarget (upstream branch: the
// HOSTNAME, so the corp proxy resolves and dials) and forwards over the
// shared transport whose DialContext CONNECTs through that proxy
// (dialThroughUpstream) — but nothing combined the two harnesses that would
// PROVE it: upstream_test.go's startFakeUpstream (a real CONNECT proxy that
// captures the CONNECT line) and mitm_test.go's MITM machinery. Every MITM
// test until now built its proxy with Options.Dial (redirectDial) and never
// set Options.Upstream, which is exactly why this was never pinned.
//
// The MITM'd host here takes the "portal.sso" shape: an operator's estate
// with a PrivateLink-style SSO portal resolves that hostname to CGNAT space
// (100.64.0.0/10), which is why an operator has to declare it under
// SiteConfig.InternalHosts (the internal-host lift) for the corp-upstream
// branch of egressTarget/vetHost to admit it at all — the same wiring these
// tests exercise.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mitmUpstreamHost is the operator's own MITM-eligible corp host, in the
// "portal.sso" shape the field report describes — a corp artifact/portal
// host (isCorpMITMHost), never one of the built-in LLM hosts.
const mitmUpstreamHost = "portal.sso.eu-west-2.amazonaws.com"

// mitmUpstreamCGNAT is the address the operator's estate resolves
// mitmUpstreamHost to — CGNAT (RFC 6598), private/reserved (blockPrivate) and
// therefore only reachable via the SiteConfig.InternalHosts lift.
const mitmUpstreamCGNAT = "100.64.9.9"

// connectAndHandshakeMITM drives the agent side of a MITM'd CONNECT to host,
// generalizing mitm_test.go's agentMITMConn (which hardcodes anthropicHost)
// to an arbitrary corp-artifact host.
func connectAndHandshakeMITM(t *testing.T, proxyURL, host string, caPEM []byte) *tls.Conn {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(proxyURL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(conn, "CONNECT "+host+":443 HTTP/1.1\r\nHost: "+host+":443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("CONNECT status = %d, want 200", resp.StatusCode)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatal("failed to add Wardyn CA to agent trust pool")
	}
	tlsConn := tls.Client(conn, &tls.Config{ServerName: host, RootCAs: pool})
	if err := tlsConn.Handshake(); err != nil {
		t.Fatalf("agent TLS handshake (must trust the Wardyn CA leaf): %v", err)
	}
	return tlsConn
}

// TestMITMUpstreamDialsRealHostnameThroughCorpProxy is the pin: a MITM'd
// tunnel to an operator-configured corp host, with an upstream corp proxy
// configured, must re-originate its forwarded request as CONNECT
// <real-host>:443 to the FAKE CORP PROXY — by name, never a resolved IP
// literal, exactly as TestGitBrokerDialsGithubByNameThroughUpstream and
// TestLLMRouteDialsByNameThroughUpstream already pin for the two brokered
// routes. The resolver answers the host with a CGNAT address (the
// "portal.sso" shape the field report describes) and InternalHosts declares
// it, so the request threads the SAME vetHost/internal-host lift the direct
// (non-MITM) upstream branch already has coverage for.
//
// SCOPE NOTE: proving the injected header lands at the ORIGIN needs a
// TLS-terminating fake behind the fake corp proxy; both startFakeUpstream and
// mitm_test.go's stand-ins are raw CONNECT-and-echo stubs, so that is new
// work, not a copy, and is out of scope here (see the lane spec). The
// CONNECT-line assertion below — proving the re-originated dial reached the
// corp proxy, by name, instead of going direct — settles the question the
// field report raised.
func TestMITMUpstreamDialsRealHostnameThroughCorpProxy(t *testing.T) {
	f := startFakeUpstream(t)
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	res := fakeResolver{m: map[string][]net.IP{mitmUpstreamHost: ips(mitmUpstreamCGNAT)}}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 32)}

	p := newProxy(Options{
		RunID:         uuid.New(),
		Policy:        CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{mitmUpstreamHost}}),
		Sink:          sink,
		CA:            ca,
		MITMHosts:     []string{mitmUpstreamHost}, // bare host: corp-artifact MITM, not an LLM host
		Resolver:      res,
		Upstream:      up,
		InternalHosts: []types.InternalHost{{HostSuffix: mitmUpstreamHost}}, // lifts the CGNAT answer
		// Dial is deliberately left nil (the production net.Dialer): the fake
		// corp proxy listens on a real loopback socket, exactly as
		// upstream_test.go's own tests reach it with no Dial override.
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := connectAndHandshakeMITM(t, proxySrv.URL, mitmUpstreamHost, certPEM)
	defer tlsConn.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://"+mitmUpstreamHost+"/federation/credentials", nil)
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	// The fake corp proxy is a raw CONNECT-and-echo stub (no real TLS
	// endpoint behind it), so the re-originated request's own TLS handshake
	// fails and serveMITMRequest 502s — exactly as
	// TestGitBrokerDialsGithubByNameThroughUpstream documents. That failure
	// is expected and irrelevant: the CONNECT to the corp proxy has already
	// happened (and been recorded) by the time it occurs. Read (and ignore
	// errors on) the response so nothing is left blocked on an unread write.
	_, _ = http.ReadResponse(bufio.NewReader(tlsConn), req)

	gotConnect, _ := f.snapshot()
	if gotConnect != "CONNECT "+mitmUpstreamHost+":443" {
		t.Fatalf("corp proxy saw %q, want CONNECT %s:443 (the real hostname, not a resolved IP literal — "+
			"a corp proxy's own hostname allowlist would refuse a literal CONNECT authority)", gotConnect, mitmUpstreamHost)
	}

	// The vetHost/internal-host lift actually ran on this path (not merely
	// "some allow"): the CONNECT's own evaluate() decision is attributed to
	// the internal-host lift, proving the CGNAT resolver answer was
	// consulted and admitted via InternalHosts rather than some other route.
	d := findDecision(t, buf, ruleSourceInternalHost)
	if d.Request.Host != mitmUpstreamHost {
		t.Fatalf("internal-host decision host = %q, want %s", d.Request.Host, mitmUpstreamHost)
	}
}

// TestMITMUpstreamBypassGoesDirect pins the OTHER candidate the field report
// did not name: with the AWS suffix on SiteConfig.UpstreamProxyNoProxy, the
// SAME MITM'd dial must go DIRECT (never touching the corp proxy at all), and
// — because bypassing the corp upstream never lifts the SSRF guard
// (bypassUpstream's own doc) — it is still subject to the private-IP guard /
// InternalHosts lift exactly as the corp-upstream branch is. Proving this
// matters because "wire the upstream" is not the only way this lane could
// have been broken: a bypass list that quietly also skipped the vet, or that
// still routed through the corp proxy despite naming a no_proxy suffix, would
// both make the operator's ORIGINAL diagnosis right by a mechanism they never
// named.
func TestMITMUpstreamBypassGoesDirect(t *testing.T) {
	f := startFakeUpstream(t) // must stay UNTOUCHED for this test to mean anything
	up, err := parseUpstreamProxy("http://" + f.addr())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	certPEM, keyPEM := genTestCA(t)
	ca, err := newCertAuthority(certPEM, keyPEM)
	if err != nil {
		t.Fatalf("newCertAuthority: %v", err)
	}
	res := fakeResolver{m: map[string][]net.IP{mitmUpstreamHost: ips(mitmUpstreamCGNAT)}}
	buf := &bytes.Buffer{}
	sink := &decisionSink{out: buf, ch: make(chan egress.DecisionLog, 32)}

	// dialStub records the address the DIRECT dial (the bypass branch) was
	// asked to reach and refuses it — no real destination is needed: the
	// assertions below are about WHICH hop was asked, not whether it
	// succeeds. Because Options.Dial replaces p.dial itself, it would ALSO
	// intercept a (wrongly) corp-proxied dial; the corp fake's own accept
	// counter below is the INDEPENDENT proof that path was never taken.
	var mu sync.Mutex
	var gotDialAddr string
	dialStub := func(_ context.Context, _, addr string) (net.Conn, error) {
		mu.Lock()
		gotDialAddr = addr
		mu.Unlock()
		return nil, errors.New("test stub: no real direct destination")
	}

	p := newProxy(Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{mitmUpstreamHost}}),
		Sink:            sink,
		CA:              ca,
		MITMHosts:       []string{mitmUpstreamHost},
		Resolver:        res,
		Upstream:        up,
		UpstreamNoProxy: []string{"amazonaws.com"}, // the bypass: routes AROUND the corp proxy
		InternalHosts:   []types.InternalHost{{HostSuffix: mitmUpstreamHost}},
		Dial:            dialStub,
	})
	proxySrv := httptest.NewServer(p)
	defer proxySrv.Close()

	tlsConn := connectAndHandshakeMITM(t, proxySrv.URL, mitmUpstreamHost, certPEM)
	defer tlsConn.Close()

	req, _ := http.NewRequest(http.MethodGet, "https://"+mitmUpstreamHost+"/federation/credentials", nil)
	if err := req.Write(tlsConn); err != nil {
		t.Fatal(err)
	}
	_, _ = http.ReadResponse(bufio.NewReader(tlsConn), req) // the stub dial fails; expected, see above

	if gotConnect, _ := f.snapshot(); gotConnect != "" {
		t.Fatalf("corp proxy saw %q; a bypassed (no_proxy) host must never reach the corp proxy at all", gotConnect)
	}
	if got := atomic.LoadInt32(&f.accepts); got != 0 {
		t.Fatalf("corp proxy accepted %d connection(s); a bypassed host must never even dial it", got)
	}

	mu.Lock()
	addr := gotDialAddr
	mu.Unlock()
	wantAddr := net.JoinHostPort(mitmUpstreamCGNAT, "443")
	if addr != wantAddr {
		t.Fatalf("direct dial target = %q, want %q — the bypass must still hand the DIRECT dial the "+
			"vetHost-resolved, InternalHosts-lifted address, exactly as the non-upstream path does", addr, wantAddr)
	}

	// The CONNECT's own evaluate() decision still names the internal-host
	// lift: bypassing the corp proxy is a ROUTING choice only, and never
	// lifts the SSRF guard on its own (bypassUpstream's doc) — the lift
	// still has to come from InternalHosts, on this path exactly as on the
	// corp-upstream one.
	d := findDecision(t, buf, ruleSourceInternalHost)
	if d.Request.Host != mitmUpstreamHost {
		t.Fatalf("internal-host decision host = %q, want %s", d.Request.Host, mitmUpstreamHost)
	}
}
