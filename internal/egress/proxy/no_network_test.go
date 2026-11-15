// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A run with no network (Config.NoNetwork) reaches nothing on any transport the sandbox can use: each
// request is refused at evaluate's step 0 (or before the via_org forward, which never reaches
// evaluate), and the egress dial and the CONNECT dial refuse as a backstop. The proxy's own calls to the
// control plane still go through.

// noNetworkProxy is an allow-all proxy with no network, whose every dial lands on a counting upstream.
func noNetworkProxy(t *testing.T) (*Proxy, *syncBuf, *atomic.Int32) {
	t.Helper()
	upstream, dials := countingUpstream(t)
	p, buf := newRuleTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, upstream, false, "", nil)
	p.noNetwork = true
	return p, buf, dials
}

func wantNoNetworkRow(t *testing.T, label, rows string) {
	t.Helper()
	if !strings.Contains(rows, `"rule_source":"`+ruleSourceNoNetwork+`"`) {
		t.Errorf("%s: no %s row in %s", label, ruleSourceNoNetwork, rows)
	}
}

func TestNoNetwork_EveryTransportIsRefused(t *testing.T) {
	t.Run("CONNECT", func(t *testing.T) {
		p, buf, dials := noNetworkProxy(t)
		srv := httptest.NewServer(p)
		defer srv.Close()
		conn, resp := connectThrough(t, srv.URL, "public.test:443")
		defer conn.Close()
		if !strings.Contains(resp, " 403 ") || dials.Load() != 0 {
			t.Fatalf("CONNECT answered %q with %d dials, want 403 and none", resp, dials.Load())
		}
		wantNoNetworkRow(t, "CONNECT", buf.String())
	})
	t.Run("absolute-URI HTTP", func(t *testing.T) {
		p, buf, dials := noNetworkProxy(t)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://public.test/x"))
		if rec.Code != http.StatusForbidden || dials.Load() != 0 {
			t.Fatalf("plain HTTP = %d with %d dials, want 403 and none", rec.Code, dials.Load())
		}
		// The conformance case tells this refusal from every other deny by this header.
		if got := rec.Header().Get(egressHeaderReason); got != ruleSourceNoNetwork {
			t.Errorf("plain HTTP %s = %q, want %q", egressHeaderReason, got, ruleSourceNoNetwork)
		}
		wantNoNetworkRow(t, "plain HTTP", buf.String())
	})
	t.Run("an IP literal", func(t *testing.T) {
		p, buf, dials := noNetworkProxy(t)
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, mustProxyReq(t, http.MethodGet, "http://8.8.8.8/x"))
		if rec.Code != http.StatusForbidden || dials.Load() != 0 {
			t.Fatalf("plain HTTP to a literal = %d with %d dials, want 403 and none", rec.Code, dials.Load())
		}
		wantNoNetworkRow(t, "literal", buf.String())
	})
	t.Run("kubectl door", func(t *testing.T) {
		h := newKubeHarness(t, kubeOpts{trustCA: true})
		h.p.noNetwork = true
		if rec := h.tunnel(t, http.MethodGet, "/api/v1/namespaces/team-a/pods"); rec.Code == http.StatusOK {
			t.Fatalf("the kubectl door answered 200: %s", rec.Body)
		}
		if seen := h.upstreamSaw(); len(seen) != 0 {
			t.Fatalf("the API server saw %q", seen)
		}
	})
	t.Run("PAT door", func(t *testing.T) {
		h := newAPIHarness(t, types.PATForgeGitLab, apiGrants[types.PATForgeGitLab])
		h.p.noNetwork = true
		if rec := h.serve(t, apiReq{"GET", glBase + "/issues", "", "", nil}); rec.Code == http.StatusOK || h.forgeHits() != 0 {
			t.Fatalf("the PAT door answered %d with %d forge hits, want a refusal and none", rec.Code, h.forgeHits())
		}
	})
	t.Run("MCP broker", func(t *testing.T) {
		f := newMCPFixture(t, types.RunPolicySpec{}, nil, nil)
		f.p.noNetwork = true
		rec := f.post(t, mcpToken("claude-code"), "github", mcpInit)
		if rec.Code != http.StatusForbidden || f.up.hitCount() != 0 || len(f.dialled()) != 0 {
			t.Fatalf("MCP = %d, hits %d, dialled %v; want 403 and nothing", rec.Code, f.up.hitCount(), f.dialled())
		}
		wantDecision(t, f.buf, egress.Deny, ruleSourceNoNetwork)
	})
	t.Run("VM guest flow", func(t *testing.T) {
		upstream, dials := countingUpstream(t)
		p, buf := newRuleTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}}, upstream, true, "", nil)
		p.noNetwork = true
		// A flow parked before the switch was read resumes past evaluate: the CONNECT dial refuses it.
		p.vmFlows.park(flowB, parkedFlow{host: "origin.test", port: 443, target: "93.184.216.34:443",
			log: decisionLog(egress.Request{RunID: p.runID, Host: "origin.test", Port: 443, Method: http.MethodConnect}, egress.Allow, "policy:allowed"), at: time.Now()})
		srv := relayServer(p)
		defer srv.Close()
		addr := strings.TrimPrefix(srv.URL, "http://")
		if st, _, _ := stagedConnect(t, addr, "origin.test:443", flowA, vmnetStageAsk); st == http.StatusNoContent || st == http.StatusOK {
			t.Fatalf("ask = %d, want a refusal", st)
		}
		if st, _, _ := stagedConnect(t, addr, "origin.test:443", flowB, vmnetStageTunnel); st == http.StatusOK {
			t.Fatalf("a parked flow's tunnel = %d, want the dial refused", st)
		}
		if dials.Load() != 0 {
			t.Fatalf("%d dials, want none", dials.Load())
		}
		wantNoNetworkRow(t, "VM ask", buf.String())
	})
	t.Run("desktop sign-in", func(t *testing.T) {
		rig := newSigninLaneRig(t, func(o *Options) { o.NoNetwork = true })
		if _, _, status := rig.connect(t, rig.signinAddr, "site.test:443", signinAuth(rig.goodCred)); status != http.StatusForbidden {
			t.Fatalf("sign-in CONNECT = %d, want 403", status)
		}
		if rig.upstream.reached {
			t.Fatal("the sign-in listener reached the site")
		}
		wantNoNetworkRow(t, "sign-in", rig.logs.String())
	})
	t.Run("via_org", func(t *testing.T) {
		f := newLaptopFixture(t)
		f.p.noNetwork = true
		srv := httptest.NewServer(f.p)
		defer srv.Close()
		req, _ := http.NewRequest(http.MethodPost, "http://api.corp.example/v1/messages", strings.NewReader(`{"q":1}`))
		if resp := proxyDo(t, srv.URL, req); resp.StatusCode != http.StatusForbidden {
			t.Fatalf("plain via_org = %d, want 403", resp.StatusCode)
		}
		gw := httptest.NewRecorder()
		f.p.ServeHTTP(gw, httptest.NewRequest(http.MethodGet, "/wardyn/gh/acme/app.git/info/refs?service=git-upload-pack", nil))
		if gw.Code != http.StatusForbidden {
			t.Fatalf("via_org git broker = %d, want 403", gw.Code)
		}
		if f.cpSeen != 0 || f.lHits != 0 {
			t.Fatalf("the organisation saw %d, the laptop dialed %d; want none", f.cpSeen, f.lHits)
		}
		_ = f.p.sink.close(context.Background())
		wantNoNetworkRow(t, "via_org", f.sink.String())

		// The organisation's side refuses too: its engine is built from the run's config.
		o := newViaOrgFixture(t, nil, func(c *Config) { c.NoNetwork = true })
		if w := o.inject(http.MethodPost, "/v1/messages", nil); w.Code != http.StatusForbidden {
			t.Fatalf("org engine = %d, want 403", w.Code)
		}
		if hits, _, _ := o.up.seen(); hits != 0 {
			t.Fatalf("the org engine dialed %d times", hits)
		}
	})
}

// TestNoNetwork_EgressDialRefuses: the egress transport every door rides refuses the dial itself, so a
// door that reached it without evaluate still dials nothing.
func TestNoNetwork_EgressDialRefuses(t *testing.T) {
	p, _, dials := noNetworkProxy(t)
	req, _ := http.NewRequestWithContext(context.WithValue(context.Background(), vettedIPKey{}, "93.184.216.34:80"), http.MethodGet, "http://public.test/", nil)
	resp, err := p.roundTripUpstream(req)
	if err == nil {
		_ = resp.Body.Close()
	}
	if !errors.Is(err, errNoNetwork) || dials.Load() != 0 {
		t.Fatalf("round trip err = %v with %d dials, want %v and none", err, dials.Load(), errNoNetwork)
	}
}

// TestNoNetwork_ControlPlaneStillReached: the control-plane client shares directDial, which the switch
// never touches, so a run with no network still reaches its control plane.
func TestNoNetwork_ControlPlaneStillReached(t *testing.T) {
	var auth atomic.Value
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth.Store(r.Header.Get("Authorization"))
		_, _ = io.WriteString(w, "ok")
	}))
	defer cp.Close()
	p := newProxy(Options{
		RunID: uuid.New(), Policy: CompilePolicy(types.RunPolicySpec{}), Resolver: publicResolver{},
		Sink:            &decisionSink{out: &syncBuf{}, ch: make(chan egress.DecisionLog, 64)},
		ControlPlaneURL: cp.URL, RunToken: newTokenSource("RUNTOK"), NoNetwork: true,
	})
	resp, err := p.forwardToControlPlane(context.Background(), http.MethodGet, "/api/v1/internal/ping", nil, "")
	if err != nil {
		t.Fatalf("control plane call: %v", err)
	}
	defer resp.Body.Close()
	if body, _ := io.ReadAll(resp.Body); resp.StatusCode != http.StatusOK || string(body) != "ok" || auth.Load() != "Bearer RUNTOK" {
		t.Fatalf("control plane answered %d %q (auth %v), want 200 ok with the run token", resp.StatusCode, body, auth.Load())
	}
}

// TestNoNetwork_ConfigKey: no_network is the key the control plane sends, and it reaches the proxy.
func TestNoNetwork_ConfigKey(t *testing.T) {
	cfg, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:8443","run_token":"RUNTOK","policy":{"allowed_domains":[]},"no_network":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.NoNetwork {
		t.Fatal("no_network did not decode")
	}
}

// TestNoNetwork_NoUpstreamHop: a run with no network has no upstream proxy hop, whoever put the URL in its config
// (a Docker runner adds its own to every run, and an older server sends the organisation's). The URL's credential
// is still masked, and NewServer leaves Proxy.upstream nil, so newProxy writes no builtin:upstream-proxy allow at
// start. The same config without no_network keeps the hop.
func TestNoNetwork_NoUpstreamHop(t *testing.T) {
	const pass = "nn-hop-s3cret-4e7a19c2" // no other test's value: the mask registry is process-wide
	upstream := "http://corpuser:" + pass + "@proxy.corp.example:8080"
	if up, err := upstreamProxyFor(context.Background(), &Config{UpstreamProxyURL: upstream, NoNetwork: true}, uuid.Nil); err != nil || up != nil {
		t.Fatalf("upstreamProxyFor with no network = %v, %v; want no hop and no error", up, err)
	}
	if masked := maskDecisionBytes([]byte(`{"upstream_pw":"` + pass + `"}`)); strings.Contains(string(masked), pass) {
		t.Errorf("the dropped hop's credential is not masked: %s", masked)
	}
	for _, noNetwork := range []bool{true, false} {
		cfg, err := LoadConfigBytes([]byte(`{"run_id":"` + uuid.NewString() + `","control_plane_url":"http://127.0.0.1:1","run_token":"tok","listen":"127.0.0.1:0",` +
			`"policy":{"allowed_domains":[]},"upstream_proxy_url":"` + upstream + `","no_network":` + map[bool]string{true: "true", false: "false"}[noNetwork] + `}`))
		if err != nil {
			t.Fatalf("LoadConfigBytes (no_network %v): %v", noNetwork, err)
		}
		srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: time.Second}, io.Discard)
		if err != nil {
			t.Fatalf("NewServer (no_network %v): %v", noNetwork, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = srv.Shutdown(ctx)
		})
		if hop := srv.proxy.upstream != nil; hop == noNetwork {
			t.Errorf("no_network %v: Proxy.upstream set = %v, want %v", noNetwork, hop, !noNetwork)
		}
	}
}
