// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The proxy's own injector renewing a service-grant header across the token's expiry, against a real Postgres:
// the run launched through POST /runs, the proxy's injector built from the dispatched injection entry, its
// resolves sent over HTTP to the authenticated injection route with the run's token, and the header observed
// where the injector forwards it. Guarded by WARDYN_TEST_PG; skipped when unset.

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The injector reads the real clock and the server reads cfg.Now, so the test moves the server's clock instead of
// waiting: a resolve answered with the server six minutes behind is a resolve made six minutes ago, and moving the
// server 51 minutes past the first mint is the moment the proxy asks again near the token's end. The injector's
// boot resolve installs a lease already inside its five-minute margin, so the first forward resolves again and
// gets the server's cached token; that answer arms the injector's retry pacing (proxy.injector.resolveCtx), which
// holds the header until the pacing runs out and the injector asks; the server must answer that ask with a new
// token, never the one inside its last ten minutes.
func TestServiceGrant_ProxyRenewsAcrossExpiry_PG(t *testing.T) {
	f := newSvcLaunchPG(t)
	var skew atomic.Int64
	skew.Store(int64(-6 * time.Minute))
	f.h.srv.cfg.Now = func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }
	_, spec := f.launch(t)
	inj := f.injection(t, spec)
	if spec.ProxyConfig.RunToken == "" || f.idp.count() != 1 {
		t.Fatalf("launch: run token set=%v, token requests=%d; want a token and dispatch's one mint",
			spec.ProxyConfig.RunToken != "", f.idp.count())
	}

	var resolves atomic.Int32
	cp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/v1/internal/injection/") {
			resolves.Add(1)
		}
		f.h.srv.Handler().ServeHTTP(w, r)
	}))
	t.Cleanup(cp.Close)
	up := newViaOrgUpstream(t)
	host := inj.Rule.Host
	eng, err := proxy.NewViaOrgEngine(context.Background(), &proxy.Config{
		RunID: spec.RunID, ControlPlaneURL: cp.URL, RunToken: spec.ProxyConfig.RunToken, DecisionBufferSize: 16,
		Policy:       types.RunPolicySpec{AllowedDomains: []string{host}},
		Injection:    []proxy.InjectionConfig{{InjectionRule: inj.Rule, GrantID: inj.GrantID}},
		TrustedCAPEM: up.caPEM,
	}, proxy.ViaOrgEngineOptions{
		Client: &http.Client{Timeout: 10 * time.Second},
		Dial: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, strings.TrimPrefix(up.srv.URL, "https://"))
		},
		LookupIP: func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil },
	})
	if err != nil {
		t.Fatalf("build the injector over the dispatched entry: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = eng.Close(ctx)
	})
	if n := resolves.Load(); n != 1 {
		t.Fatalf("resolves after the injector was built = %d, want its one boot resolve", n)
	}
	// send is one sandbox request through the injector; it answers the header the upstream received.
	send := func(what string) string {
		t.Helper()
		before, _, _ := up.seen()
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"q":1}`))
		r.Host, r.RequestURI = host, ""
		w := httptest.NewRecorder()
		eng.ServeInjected(w, r, host)
		hits, auth, _ := up.seen()
		if w.Code != http.StatusOK || hits != before+1 {
			t.Fatalf("%s: %d %s with %d upstream hits, want one forwarded request", what, w.Code, w.Body.String(), hits-before)
		}
		return auth[len(auth)-1]
	}
	first, second := "Bearer "+svcToken(1), "Bearer "+svcToken(2)

	if got := send("the first request"); got != first {
		t.Fatalf("the first request forwarded %q, want the dispatched token", got)
	}
	if n := resolves.Load(); n != 2 || f.idp.count() != 1 {
		t.Fatalf("after the first request: %d resolves and %d token requests, want 2 (the lease was inside the margin) and 1",
			n, f.idp.count())
	}

	// 51 minutes after the first mint: nine minutes are left on the first token, so the server must renew it.
	skew.Store(int64(45 * time.Minute))
	var forwarded []string
	for deadline := time.Now().Add(90 * time.Second); ; {
		got := send("a request near the token's end")
		forwarded = append(forwarded, got)
		if got != first {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the injector forwarded the first token for %d requests over 90 s and never renewed it", len(forwarded))
		}
		time.Sleep(time.Second) // real time for the injector's retry pacing to run out; the loop is bounded above
	}
	renewal := resolves.Load()
	if got := forwarded[len(forwarded)-1]; got != second || renewal != 3 || f.idp.count() != 2 {
		t.Fatalf("the renewal forwarded %q after %d resolves and %d token requests, want the second token, 3 and 2",
			got, renewal, f.idp.count())
	}
	if got := send("the request after the renewal"); got != second || resolves.Load() != renewal {
		t.Fatalf("after the renewal the injector forwarded %q with %d more resolves, want the second token from its own copy",
			got, resolves.Load()-renewal)
	}
	_, auth, _ := up.seen()
	if i := slices.Index(auth, second); i < 0 || slices.Contains(auth[i:], first) {
		t.Fatalf("forwarded headers %v: the first token followed its replacement", auth)
	}
}
