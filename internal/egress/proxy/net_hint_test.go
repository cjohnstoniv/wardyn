// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// netHintForge stands in for a forge behind a corporate resolver: a TLS server
// whose certificate names forgeHost under a corp CA the proxy is configured to
// trust (the git broker and a CONNECT dial it on 443), and a plain server that
// answers the control plane's mint and plain-HTTP REST on any other address.
type netHintForge struct {
	caPEM  string
	tlsSrv *httptest.Server
	plain  *httptest.Server
}

const netHintForgeHost = "dev.azure.com"

func newNetHintForge(t *testing.T) *netHintForge {
	t.Helper()
	caCert, caKey := genTestCA(t)
	f := &netHintForge{caPEM: string(caCert)}
	f.tlsSrv = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "git-pack-data")
	}))
	f.tlsSrv.TLS = &tls.Config{Certificates: []tls.Certificate{genLeafSignedBy(t, caCert, caKey, netHintForgeHost)}}
	f.tlsSrv.StartTLS()
	t.Cleanup(f.tlsSrv.Close)
	exp := time.Now().Add(time.Hour).UTC().Format(time.RFC3339)
	f.plain = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v1/internal/credentials/mint" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"git_pat","token":"T","username":"pat","jti":"j","expires_at":"`+exp+`"}`)
			return
		}
		_, _ = io.WriteString(w, "rest-ok")
	}))
	t.Cleanup(f.plain.Close)
	return f
}

// dial routes a vetted :443 target to the TLS forge and everything else (the
// control plane, a plain-HTTP REST call) to the plain server.
func (f *netHintForge) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	to := upstreamAddr(f.plain)
	if strings.HasSuffix(addr, ":443") {
		to = upstreamAddr(f.tlsSrv)
	}
	return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, network, to)
}

// boot starts the runner's proxy from a dispatch-rendered golden, with the PAT
// grant the run holds for the forge host (§1 of the field report, which works)
// and the corp CA added; the resolver answers the forge host with addr.
func (f *netHintForge) boot(t *testing.T, golden, addr string) (*Server, *bytes.Buffer) {
	t.Helper()
	cfg, err := LoadConfig(golden)
	if err != nil {
		t.Fatal(err)
	}
	cfg.PATGrants = map[string]PATGrant{netHintForgeHost: {GrantID: uuid.New()}}
	cfg.TrustedCAPEM = f.caPEM
	buf := &bytes.Buffer{}
	srv, err := newServer(context.Background(), cfg, &http.Client{Timeout: time.Second}, buf,
		serverHooks{res: fakeResolver{m: map[string][]net.IP{netHintForgeHost: ips(addr)}}, dial: f.dial})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = srv.Shutdown(context.Background()) })
	return srv, buf
}

// netHintWant is the sentence the field report asked for, written out here
// rather than read from internalHostsLiftHint, so a change to it fails a pin.
func netHintWant(host, block string) string {
	return "an operator may lift this guard for " + host + " with site_config.internal_hosts, scoped to " + block
}

const netHintGitPath = "/wardyn/git/" + netHintForgeHost + "/org/proj/_git/repo/info/refs?service=git-upload-pack"

// TestNetHint_PrivateForgeOnARunner is the field report's repro, end to end. A
// self-hosted runner's resolver answers the forge host into CGNAT, or RFC1918.
// With the organisation's dispatch rendering no internal_hosts, the brokered git
// lane, a CONNECT and a plain-HTTP REST call are each refused, and each refusal
// (the sandbox's text and the decision row the console shows) names the remedy:
// that host, site_config.internal_hosts, and the block the address fell in,
// never the address. With dispatch rendering that declaration (the goldens
// internal/api TestNetHintDispatchWireConfig pins across the runner wire), all
// three are admitted, and an address outside the declared block still is not.
func TestNetHint_PrivateForgeOnARunner(t *testing.T) {
	for _, c := range []struct {
		name, addr, block, lifted, otherAddr, otherBlock string
	}{
		{"cgnat", "100.64.0.10", "100.64.0.0/10", "testdata/dispatched_net_hint_cgnat.json", "10.0.0.10", "10.0.0.0/8"},
		{"rfc1918", "10.0.0.10", "10.0.0.0/8", "testdata/dispatched_net_hint_rfc1918.json", "100.64.0.10", "100.64.0.0/10"},
	} {
		t.Run(c.name, func(t *testing.T) {
			hint := netHintWant(netHintForgeHost, c.block)
			f := newNetHintForge(t)

			// Refused, naming the remedy, on all three paths.
			srv, buf := f.boot(t, "testdata/dispatched_net_hint_undeclared.json", c.addr)
			assertNetHintRefusals(t, srv, buf, hint, c.addr)

			// The declaration, delivered through dispatch: admitted on all three.
			srv, buf = f.boot(t, c.lifted, c.addr)
			rec := httptest.NewRecorder()
			srv.proxy.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, netHintGitPath, nil))
			if d := lastDecision(t, buf); rec.Code != http.StatusOK || rec.Body.String() != "git-pack-data" || d.Decision != egress.Allow {
				t.Fatalf("git lane, declared: status %d body %q row %+v; want 200 git-pack-data, allow", rec.Code, rec.Body.String(), d)
			}
			rec = httptest.NewRecorder()
			srv.proxy.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+netHintForgeHost+"/org/_apis/projects"))
			if d := lastDecision(t, buf); rec.Code != http.StatusOK || rec.Body.String() != "rest-ok" || d.RuleSource != ruleSourceInternalHost {
				t.Fatalf("REST, declared: status %d body %q row %+v; want 200 rest-ok, %s", rec.Code, rec.Body.String(), d, ruleSourceInternalHost)
			}
			front := httptest.NewServer(srv.proxy)
			defer front.Close()
			conn, status := connectThrough(t, front.URL, netHintForgeHost+":443")
			_ = conn.Close()
			if !strings.HasPrefix(status, "HTTP/1.1 200") {
				t.Fatalf("CONNECT, declared: %q, want 200", status)
			}

			// The lift is scoped to the declared block: the other range stays refused.
			srv, buf = f.boot(t, c.lifted, c.otherAddr)
			assertNetHintRefusals(t, srv, buf, netHintWant(netHintForgeHost, c.otherBlock), c.otherAddr)
		})
	}
}

// assertNetHintRefusals drives the git lane, a CONNECT and a plain-HTTP REST
// call at the forge host and asserts each is a 403 whose text and decision row
// carry hint, and whose text never carries addr.
func assertNetHintRefusals(t *testing.T, srv *Server, buf *bytes.Buffer, hint, addr string) {
	t.Helper()
	check := func(path string, rec *httptest.ResponseRecorder, wantSource string) {
		t.Helper()
		d := lastDecision(t, buf)
		if rec.Code != http.StatusForbidden || d.Decision != egress.Deny || d.RuleSource != wantSource {
			t.Fatalf("%s: status %d row %+v; want 403, deny %s", path, rec.Code, d, wantSource)
		}
		body := rec.Body.String()
		if !strings.Contains(body, hint) || d.Cause != hint {
			t.Fatalf("%s: body %q cause %q; want both to name %q", path, body, d.Cause, hint)
		}
		if strings.Contains(body, addr) || strings.Contains(d.Cause, addr) {
			t.Fatalf("%s: the refusal shows the address %s: body %q cause %q", path, addr, body, d.Cause)
		}
	}
	rec := httptest.NewRecorder()
	srv.proxy.ServeHTTP(rec, mustLocalReq(t, http.MethodGet, netHintGitPath, nil))
	check("git lane", rec, ruleSourcePATDenied)

	rec = httptest.NewRecorder()
	srv.proxy.ServeHTTP(rec, connectReq(t, netHintForgeHost+":443"))
	check("CONNECT", rec, "builtin:private-ip")
	if got := rec.Header().Get(egressHeaderDetail); !strings.Contains(got, hint) {
		t.Fatalf("CONNECT: %s = %q, want it to name %q", egressHeaderDetail, got, hint)
	}
	var b struct {
		Wardyn struct {
			Detail string `json:"detail"`
		} `json:"wardyn"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &b); err != nil || !strings.Contains(b.Wardyn.Detail, hint) {
		t.Fatalf("CONNECT: 403 body detail %q (%v), want it to name %q", b.Wardyn.Detail, err, hint)
	}

	rec = httptest.NewRecorder()
	srv.proxy.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://"+netHintForgeHost+"/org/_apis/projects"))
	check("REST", rec, "builtin:private-ip")
}

// TestNetHint_OnlyWhereInternalHostsCanLift: the hint rides a hostname refused
// for RFC1918/ULA/CGNAT space and nothing else. A never-liftable class, a
// literal address (whose way in is an exact allowed_domains entry), and an
// address on the proxy's own subnet (which liftInternalHost refuses whatever is
// declared) keep their own text and carry none.
func TestNetHint_OnlyWhereInternalHostsCanLift(t *testing.T) {
	_, own, _ := net.ParseCIDR("192.168.7.0/24")
	p := newProxy(Options{
		RunID:  uuid.New(),
		Policy: CompilePolicy(types.RunPolicySpec{}),
		Resolver: fakeResolver{m: map[string][]net.IP{
			"priv.test": ips("10.0.0.10"), "ula.test": ips("fd00::10"), "own.test": ips("192.168.7.9"),
			"loop.test": ips("127.0.0.1"), "meta.test": ips("169.254.169.254"), "alibaba.test": ips("100.100.100.200"),
		}},
		LocalSubnets: []*net.IPNet{own},
	})
	for host, want := range map[string]string{
		"priv.test":    netHintWant("priv.test", "10.0.0.0/8"),
		"PRIV.test.":   netHintWant("priv.test", "10.0.0.0/8"),
		"ula.test":     netHintWant("ula.test", "fc00::/7"),
		"own.test":     "",
		"loop.test":    "",
		"meta.test":    "",
		"alibaba.test": "",
		"10.0.0.10":    "",
	} {
		_, _, err := p.egressTarget(host, 443)
		if err == nil {
			t.Fatalf("%s: admitted, want refused", host)
		}
		if got := liftHintOf(err); got != want {
			t.Errorf("%s: hint %q, want %q", host, got, want)
		}
		if strings.Contains(p.redactErr(err)+hintSuffix(err), "10.0.0.10") {
			t.Errorf("%s: the sandbox's text shows the address", host)
		}
	}
}

// TestNetHint_MemoSummaryRowCarriesTheHint: identical refusals answered from the
// per-run memo end in one summary row, which still names the remedy.
func TestNetHint_MemoSummaryRowCarriesTheHint(t *testing.T) {
	var mo privateIPMemo
	req := egress.Request{Host: "priv.test", Port: 443, Time: time.Now()}
	mo.record(req, blockPrivate, "the hint")
	mo.hit("priv.test", 443, time.Now())
	rows := mo.drain()
	if len(rows) != 1 || rows[0].Cause != "the hint" || rows[0].Repeat != 1 {
		t.Fatalf("summary rows = %+v, want one carrying the hint", rows)
	}
}
