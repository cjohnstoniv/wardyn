// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/vmnet"
	"github.com/cjohnstoniv/wardyn/internal/vmnet/vmnettest"
)

// vmRuleProxy is a VM run's proxy set up as dispatch sets one up for host rules: sets on hrHost and, unless
// noCA, the run's CA with hrHost:443 inspected. extra joins the allowlist, and every upstream dial reaches
// origin. It returns the proxy, its decision rows and the CA's PEM.
func vmRuleProxy(t *testing.T, noCA bool, extra []string, sets []types.HostRuleSet, origin *httptest.Server) (*Proxy, *syncBuf, []byte) {
	t.Helper()
	buf := &syncBuf{}
	opts := Options{
		RunID:           uuid.New(),
		Policy:          CompilePolicy(types.RunPolicySpec{AllowedDomains: append([]string{hrHost}, extra...), HostRules: sets}),
		Sink:            &decisionSink{out: buf, ch: make(chan queuedDecision, 64)},
		Resolver:        publicResolver{},
		Dial:            redirectDial(upstreamAddr(origin)),
		TLSClientConfig: testInsecureTLSConfig,
	}
	var caPEM []byte
	if !noCA {
		var keyPEM []byte
		caPEM, keyPEM = genTestCA(t)
		ca, err := newCertAuthority(caPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		opts.CA, opts.MITMHosts = ca, []string{hrHost + ":443"}
	}
	p := newProxy(opts)
	p.vm, p.portRule = true, effectivePortRule(true, "")
	return p, buf, caPEM
}

// vmGuestOf is a guest behind the real relay in front of p, wired as a desktop VM run is: the guest has no
// proxy setting and reaches nothing but the relay.
func vmGuestOf(t *testing.T, p *Proxy) *vmnettest.Guest {
	t.Helper()
	srv := relayServer(p)
	t.Cleanup(srv.Close)
	rl, err := vmnet.New(vmnet.Config{Proxy: strings.TrimPrefix(srv.URL, "http://")})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	return vmnettest.Attach(t, rl.Serve)
}

// ruleOrigin is the rule host's upstream: it answers each request with its method and path, and counts them.
func ruleOrigin(t *testing.T, useTLS bool) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var n atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.Add(1)
		_, _ = io.WriteString(w, r.Method+" "+r.URL.Path)
	}))
	if useTLS {
		srv.StartTLS()
	} else {
		srv.Start()
	}
	t.Cleanup(srv.Close)
	return srv, &n
}

// waitRule waits for n decision rows from rule source src (the relay reports off the guest's path).
func waitRule(t *testing.T, buf *syncBuf, src string, n int) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		got := 0
		for _, r := range rows(t, buf) {
			if r.RuleSource == src {
				got++
			}
		}
		if got >= n {
			return
		}
	}
	t.Fatalf("want %d %s rows:\n%s", n, src, buf.String())
}

// guestSend sends one request on a fresh guest flow to hrHost:port, over TLS to the proxy's certificate when
// caPEM is set, and returns the status.
func guestSend(t *testing.T, g *vmnettest.Guest, port uint16, caPEM []byte, method, path string) int {
	t.Helper()
	c, err := g.Dial(g.Resolve(t, "api", "example", "com"), port, 3*time.Second)
	if err != nil {
		t.Fatalf("%s %s: the guest's flow was refused: %v", method, path, err)
	}
	defer c.Close()
	var rw io.ReadWriter = c
	if caPEM != nil {
		pool := x509.NewCertPool()
		pool.AppendCertsFromPEM(caPEM)
		tc := tls.Client(c, &tls.Config{ServerName: hrHost, RootCAs: pool, MinVersion: tls.VersionTLS12})
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		if err := tc.Handshake(); err != nil {
			t.Fatalf("%s %s: the proxy did not open the guest's TLS with the run's CA: %v", method, path, err)
		}
		rw = tc
	} else {
		_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	}
	if _, err := io.WriteString(rw, method+" "+path+" HTTP/1.1\r\nHost: "+hrHost+"\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(rw), &http.Request{Method: method})
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// TestVMNetHostRulesJudgeEachRequestInsideTheGuestsTLS: a guest's HTTPS flow to a rule host, through the real
// relay (ask, first-flight check, tunnel), is opened by the proxy and each request in it judged. An allowed GET
// reaches the host; a refused GET or DELETE is answered 403 with a policy:host-rule row and never reaches it.
func TestVMNetHostRulesJudgeEachRequestInsideTheGuestsTLS(t *testing.T) {
	origin, reached := ruleOrigin(t, true)
	p, buf, caPEM := vmRuleProxy(t, false, nil, hrSets(), origin)
	g := vmGuestOf(t, p)
	if got := guestSend(t, g, 443, caPEM, http.MethodGet, "/public"); got != http.StatusOK {
		t.Fatalf("GET /public = %d, want 200", got)
	}
	if got := guestSend(t, g, 443, caPEM, http.MethodGet, "/admin"); got != http.StatusForbidden {
		t.Fatalf("GET /admin = %d, want 403", got)
	}
	if got := guestSend(t, g, 443, caPEM, http.MethodDelete, "/public"); got != http.StatusForbidden {
		t.Fatalf("DELETE /public = %d, want 403", got)
	}
	waitRule(t, buf, ruleSourceHostRule, 2)
	if n := reached.Load(); n != 1 {
		t.Fatalf("the rule host got %d requests, want the allowed GET alone", n)
	}
}

// TestVMNetHostRulesHoldInsideTheGuestsTLS: a hold rule holds the guest's request inside its TLS, through the
// real relay, until the approval is answered. Approved, the request reaches the host; denied, it is refused 403
// and never does. It is TestHostCallHoldInsideTheTunnel on a VM guest's flow.
func TestVMNetHostRulesHoldInsideTheGuestsTLS(t *testing.T) {
	for _, tc := range []struct {
		state types.ApprovalState
		want  int
	}{
		{types.ApprovalApproved, http.StatusOK},
		{types.ApprovalDenied, http.StatusForbidden},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			origin, reached := ruleOrigin(t, true)
			p, _, caPEM := vmRuleProxy(t, false, nil, []types.HostRuleSet{hcHoldSet()}, origin)
			cp := newHCCP(t, p)
			cp.newState = tc.state
			g := vmGuestOf(t, p)
			if got := guestSend(t, g, 443, caPEM, http.MethodPost, "/admin"); got != tc.want {
				t.Fatalf("POST /admin = %d, want %d", got, tc.want)
			}
			wantReached := int32(0)
			if tc.want == http.StatusOK {
				wantReached = 1
			}
			_, raises, _ := cp.counts()
			if n := reached.Load(); n != wantReached || raises != 1 {
				t.Fatalf("the rule host got %d requests (want %d) after %d approval raises (want 1)", n, wantReached, raises)
			}
		})
	}
}

// TestVMNetHostRulesJudgeAGuestsPlainHTTP: port 80 reaches the proxy as forward-proxy requests (vmnet's
// httpfwd.go), so the rules judge each one by its path.
func TestVMNetHostRulesJudgeAGuestsPlainHTTP(t *testing.T) {
	origin, reached := ruleOrigin(t, false)
	p, buf, _ := vmRuleProxy(t, false, nil, hrSets(), origin)
	g := vmGuestOf(t, p)
	if got := guestSend(t, g, 80, nil, http.MethodGet, "/public"); got != http.StatusOK {
		t.Fatalf("GET /public = %d, want 200", got)
	}
	if got := guestSend(t, g, 80, nil, http.MethodGet, "/admin"); got != http.StatusForbidden {
		t.Fatalf("GET /admin = %d, want 403", got)
	}
	waitRule(t, buf, ruleSourceHostRule, 1)
	if n := reached.Load(); n != 1 {
		t.Fatalf("the rule host got %d requests, want the allowed GET alone", n)
	}
}

// TestVMNetHostRulesRefuseAnUninspectedFlow: a rule host on a port the proxy does not open (an allowlisted
// host:8443), and on a run with no CA, is refused at the relay's ask with a policy:host-rule-needs-inspection
// row: never an opaque tunnel, and never a request upstream.
func TestVMNetHostRulesRefuseAnUninspectedFlow(t *testing.T) {
	for name, tc := range map[string]struct {
		noCA  bool
		extra []string
		port  uint16
	}{
		"uninspected port": {false, []string{hrHost + ":8443"}, 8443},
		"no CA":            {true, nil, 443},
	} {
		t.Run(name, func(t *testing.T) {
			origin, reached := ruleOrigin(t, true)
			p, buf, _ := vmRuleProxy(t, tc.noCA, tc.extra, hrSets(), origin)
			g := vmGuestOf(t, p)
			if c, err := g.Dial(g.Resolve(t, "api", "example", "com"), tc.port, 3*time.Second); err == nil {
				c.Close()
				t.Fatalf("the guest connected to a rule host on :%d, which the proxy cannot open", tc.port)
			}
			waitRule(t, buf, ruleSourceHostRuleNeedsInspection, 1)
			if n := reached.Load(); n != 0 {
				t.Fatalf("the rule host got %d requests", n)
			}
		})
	}
}
