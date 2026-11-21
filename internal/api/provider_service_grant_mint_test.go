// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The mint of a provider service grant: what goes on the wire, what is cached, what a failure does, and what
// the audit says. Each test drives serviceGrantToken against the fixture's scripted identity provider.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func svcErr(t *testing.T, err error) *serviceGrantError {
	t.Helper()
	var se *serviceGrantError
	if !errors.As(err, &se) {
		t.Fatalf("error = %v, want a serviceGrantError", err)
	}
	return se
}

func TestServiceGrant_ClientAuthBasicAndPost(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant.ClientID = "svc client+id/é" })
	f.writeSecret("se/cr+et with space&more=1")
	f.mustToken(serviceGrantTriggerRefresh)
	req := f.idp.requests()[0]
	id, secret := svcBasic(t, req.Header.Get("Authorization"))
	if id != "svc client+id/é" || secret != "se/cr+et with space&more=1" || req.Form.Get("client_secret") != "" || req.Form.Get("client_id") != "" {
		t.Fatalf("basic request: id=%q secret=%q form=%v, want the form-encoded pair in the header only", id, secret, req.Form)
	}
	if !strings.Contains(req.Header.Get("Authorization"), "Basic ") || req.Header.Get("Content-Type") != "application/x-www-form-urlencoded" {
		t.Errorf("headers = %v", req.Header)
	}

	f2 := newSvcFixWithSecret(t)
	f2.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant.ClientAuth = types.ServiceGrantClientAuthPost })
	f2.mustToken(serviceGrantTriggerRefresh)
	req = f2.idp.requests()[0]
	if req.Header.Get("Authorization") != "" || req.Form.Get("client_id") != svcClientID || req.Form.Get("client_secret") != svcClientSecret {
		t.Fatalf("post request: header=%q form=%v, want client_id and client_secret in the form", req.Header.Get("Authorization"), req.Form)
	}
}

func TestServiceGrant_FormCarriesScopeAndAudience(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant.Audience = "https://api.example.test/" })
	f.mustToken(serviceGrantTriggerRefresh)
	form := f.idp.requests()[0].Form
	if form.Get("grant_type") != "client_credentials" || form.Get("scope") != "api.read" || form.Get("audience") != "https://api.example.test/" {
		t.Fatalf("form = %v, want grant_type, scope and audience", form)
	}

	g := newSvcFixWithSecret(t)
	g.setRow(func(p *types.ModelProvider) { p.OIDC.ServiceGrant.Scope = "" })
	g.mustToken(serviceGrantTriggerRefresh)
	if form := g.idp.requests()[0].Form; form.Has("scope") || form.Has("audience") {
		t.Errorf("form = %v, want neither scope nor audience when the grant names none", form)
	}
}

func TestServiceGrant_NoOpenIDAndIDTokenIgnored(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		return svcAnswer{Status: 200, Body: `{"access_token":"` + svcToken(n) + `","expires_in":3600,"id_token":"idtoken-never-verified-or-kept-0001","refresh_token":"refresh-never-kept-0000000001"}`}
	}
	ids := f.captureRowIDs()
	f.mustToken(serviceGrantTriggerRefresh)
	req := f.idp.requests()[0]
	if strings.Contains(req.Form.Get("scope"), "openid") || req.Form.Has("nonce") {
		t.Errorf("form = %v, want no openid scope and no nonce", req.Form)
	}
	f.srv.svcGrant.mu.Lock()
	cached := f.srv.svcGrant.cache[f.uid]
	f.srv.svcGrant.mu.Unlock()
	if cached.token != svcToken(1) {
		t.Fatalf("cached token = %q, want the access token", cached.token)
	}
	f.awaitSink(t, ids()...) // the sweep covers the success's sink copy
	all := f.auditAll() + f.logs.String()
	for _, v := range []string{"idtoken-never-verified-or-kept-0001", "refresh-never-kept-0000000001"} {
		if strings.Contains(all, v) {
			t.Errorf("an ID token or refresh token value reached the audit or the log")
		}
	}
}

func TestServiceGrant_MixedCaseValuesSentExactly(t *testing.T) {
	f := newSvcFixWithSecret(t)
	const tokenURL = "https://idp.example.test/Tenant%2FOne/OAuth2/v2.0/Token"
	f.setRow(func(p *types.ModelProvider) {
		g := p.OIDC.ServiceGrant
		g.TokenURL, g.ClientID, g.Scope, g.Audience = tokenURL, "Client_ID-%2F-Mixed", "Api.Read Write%2Fall", "https://API.example.test/Path%2fX"
	})
	f.writeSecret(svcClientSecret)
	f.mustToken(serviceGrantTriggerRefresh)
	req := f.idp.requests()[0]
	id, _ := svcBasic(t, req.Header.Get("Authorization"))
	if req.URL != tokenURL || id != "Client_ID-%2F-Mixed" || req.Form.Get("scope") != "Api.Read Write%2Fall" || req.Form.Get("audience") != "https://API.example.test/Path%2fX" {
		t.Fatalf("sent url=%q id=%q form=%v, want every value byte for byte", req.URL, id, req.Form)
	}
}

func TestServiceGrant_TwoStatusesSameNamedError(t *testing.T) {
	f := newSvcFixWithSecret(t)
	status := 400
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(status, "invalid_client") }
	se := svcErr(t, func() error { _, err := f.token(serviceGrantTriggerRefresh); return err }())
	if se.reason != "invalid_client" || se.httpStatus != 400 || !se.definitive {
		t.Fatalf("400 answer = %+v", se)
	}
	status = 401
	se = svcErr(t, func() error { _, err := f.token(serviceGrantTriggerRefresh); return err }())
	if se.reason != "invalid_client" || se.httpStatus != 401 {
		t.Fatalf("401 answer = %+v, want its own status kept", se)
	}
	row, _ := f.st.statusOf(f.uid, f.srv.svcGrant.replica())
	if row.HTTPStatus != 401 || row.OAuthError != "invalid_client" || row.Reason != "invalid_client" {
		t.Errorf("status row = %+v", row)
	}
}

func TestServiceGrant_ShortLifetimeAndShortTokenRefused(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(n int, _ svcReq) svcAnswer { return svcOK(n, 899) }
	if se := svcErr(t, func() error { _, err := f.token(serviceGrantTriggerRefresh); return err }()); se.reason != svcReasonLifetimeTooShort || !se.definitive {
		t.Fatalf("an 899 s token = %+v, want token_lifetime_too_short", se)
	}
	f.idp.respond = func(int, svcReq) svcAnswer {
		return svcAnswer{Status: 200, Body: `{"access_token":"short","expires_in":3600}`}
	}
	if se := svcErr(t, func() error { _, err := f.token(serviceGrantTriggerRefresh); return err }()); se.reason != svcReasonTokenUnmaskable {
		t.Fatalf("a 5 byte token = %+v, want token_unmaskable", se)
	}
	f.idp.respond = func(n int, _ svcReq) svcAnswer { return svcOK(n, 900) }
	if _, err := f.token(serviceGrantTriggerRefresh); err != nil {
		t.Fatalf("a 900 s token refused: %v", err)
	}
}

func TestServiceGrant_RedirectNotFollowed(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(int, svcReq) svcAnswer {
		return svcAnswer{Status: 307, Header: http.Header{"Location": {"https://elsewhere.example.test/token"}}}
	}
	se := svcErr(t, func() error { _, err := f.token(serviceGrantTriggerRefresh); return err }())
	if f.idp.count() != 1 || se.httpStatus != 307 {
		t.Fatalf("requests = %d, error = %+v, want the one request answered and not followed", f.idp.count(), se)
	}
}

func TestServiceGrant_TokenURLRules(t *testing.T) {
	f := newSvcFix(t)
	f.srv.cfg.AllowTestEndpoints = true
	for name, url := range map[string]string{
		"plain http":     "http://idp.example.test/token",
		"a loopback IP":  "https://127.0.0.1/token",
		"userinfo":       "https://user:pw@idp.example.test/token",
		"a query":        "https://idp.example.test/token?x=1",
		"a fragment":     "https://idp.example.test/token#f",
		"no host":        "https:///token",
		"empty":          "",
		"a metadata IP":  "https://169.254.169.254/token",
		"not a URL":      "idp.example.test/token",
		"a control char": "https://idp.example.test/tok\x01en",
	} {
		p := f.provider()
		g := *p.OIDC.ServiceGrant
		g.TokenURL = url
		o := *p.OIDC
		o.ServiceGrant = &g
		p.OIDC = &o
		if err := validateServiceGrant(p); err == nil {
			t.Errorf("token_url %s (%q) accepted, want refused", name, url)
		}
	}
	if err := validateServiceGrant(f.provider()); err != nil {
		t.Errorf("the fixture's own grant refused: %v", err)
	}
}

func TestServiceGrant_ServiceIdentityEntryExactSpelling(t *testing.T) {
	f := newSvcFix(t)
	for name, ids := range map[string][]string{
		"upper case prefix": {"Service:ci-bot"}, "leading space": {" service:ci-bot"}, "trailing space": {"service:ci-bot "},
		"upper case name": {"service:CI-bot"}, "duplicate": {"service:ci-bot", "service:ci-bot"}, "empty list": {},
		"no prefix": {"ci-bot"}, "short name": {"service:ab"}, "a person": {"alice@example.com"},
	} {
		p := f.provider()
		g := *p.OIDC.ServiceGrant
		g.ServiceIdentities = ids
		o := *p.OIDC
		o.ServiceGrant = &g
		p.OIDC = &o
		if err := validateServiceGrant(p); err == nil {
			t.Errorf("service_identities %s (%q) accepted, want refused at the write", name, ids)
		}
	}
	p := f.provider()
	p.OIDC.ServiceGrant.ServiceIdentities = []string{"service:ci-bot", "service:deploy-1"}
	if err := validateServiceGrant(p); err != nil {
		t.Errorf("two exact entries refused: %v", err)
	}
}

func TestServiceGrant_MintFailureRefusesNewRun(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	ctx := context.Background()
	d, err := f.srv.providerServiceGrantRefusal(ctx, f.provider(), svcOwner, true)
	if err != nil || d.msg == "" || !d.serviceGrant || !d.credential || !strings.Contains(d.msg, "invalid_client") {
		t.Fatalf("dispatch denial = %+v, %v, want the service grant refusal naming the reason", d, err)
	}
	// Create and Review read this replica's recorded outcome and mint nothing.
	before := f.idp.count()
	d, err = f.srv.providerServiceGrantRefusal(ctx, f.provider(), svcOwner, false)
	if err != nil || !d.serviceGrant || f.idp.count() != before {
		t.Fatalf("create denial = %+v, %v with %d new requests, want the recorded refusal and no request", d, err, f.idp.count()-before)
	}
}

func TestServiceGrant_TransientFailureAnswers503(t *testing.T) {
	f := newSvcFixWithSecret(t)
	plan, _, _, ok := f.dispatch(t)
	if !ok {
		t.Fatal("dispatch refused")
	}
	f.idp.respond = func(int, svcReq) svcAnswer {
		return svcAnswer{Status: 503, Body: `{"error":"temporarily_unavailable"}`}
	}
	in := f.request(plan)
	in.sub = svcOwner
	f.clock.Advance(59*time.Minute + 30*time.Second) // the dispatch's token has 30 seconds left: none is served
	w, _ := f.resolveWith(in, "")
	if w.Code != http.StatusServiceUnavailable || svcBody(t, w)["reason"] != reasonServiceGrantUnavailable {
		t.Fatalf("resolve = %d %s, want 503 service_grant_unavailable", w.Code, w.Body.String())
	}
	f.idp.respond = func(int, svcReq) svcAnswer { return svcAnswer{Err: errors.New("connection reset")} }
	f.clock.Advance(2 * time.Minute)
	w, _ = f.resolveWith(in, "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("a transport failure answered %d, want 503", w.Code)
	}
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	f.clock.Advance(2 * time.Minute)
	w, _ = f.resolveWith(in, "")
	if w.Code != http.StatusFailedDependency || svcBody(t, w)["reason"] != reasonServiceGrantRefused {
		t.Fatalf("a definitive failure answered %d %s, want 424 service_grant_refused", w.Code, w.Body.String())
	}
}

func TestServiceGrant_DefinitiveFailureServesLiveCachedTokenOnly(t *testing.T) {
	f := newSvcFixWithSecret(t)
	first := f.mustToken(serviceGrantTriggerRefresh)
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	f.clock.Advance(55 * time.Minute) // 5 minutes of life left: inside the reuse margin, so a new request is made
	got, err := f.token(serviceGrantTriggerRefresh)
	if err != nil || got.token != first.token || f.idp.count() != 2 {
		t.Fatalf("with 5 minutes left: %q, %v after %d requests, want the live cached token", got.token, err, f.idp.count())
	}
	f.clock.Advance(4*time.Minute + 30*time.Second) // 30 seconds left
	if got, err = f.token(serviceGrantTriggerRefresh); err == nil || got.token != "" {
		t.Fatalf("with 30 seconds left: %q, %v, want the failure and no token", got.token, err)
	}
}

func TestServiceGrant_NeverServesExpiredToken(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(n int, _ svcReq) svcAnswer {
		if n == 1 {
			return svcOK(n, 900)
		}
		return svcAnswer{Status: 500}
	}
	f.mustToken(serviceGrantTriggerRefresh)
	for _, after := range []time.Duration{14 * time.Minute, time.Minute, time.Hour} {
		f.clock.Advance(after)
		if got, err := f.token(serviceGrantTriggerRefresh); err == nil && got.token != "" && !got.expiresAt.After(f.clock.Now().Add(time.Minute)) {
			t.Fatalf("after %v: handed out %q expiring %v at %v", after, got.token, got.expiresAt, f.clock.Now())
		}
	}
}

func TestServiceGrant_FailureAttemptsPaced(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	for range 3 {
		if _, err := f.token(serviceGrantTriggerDispatch); err == nil {
			t.Fatal("a failing mint succeeded")
		}
	}
	if f.idp.count() != 1 {
		t.Fatalf("3 dispatches inside a minute sent %d requests, want 1", f.idp.count())
	}
	if _, err := f.token(serviceGrantTriggerResolve); err == nil || f.idp.count() != 1 {
		t.Fatalf("a resolve inside the minute sent a request (%d)", f.idp.count())
	}
	if _, _ = f.token(serviceGrantTriggerRefresh); f.idp.count() != 2 {
		t.Fatalf("the refresher's attempt was paced (%d requests), want it sent", f.idp.count())
	}
	f.clock.Advance(61 * time.Second)
	_, _ = f.token(serviceGrantTriggerResolve)
	if f.idp.count() != 3 {
		t.Fatalf("a resolve after a minute sent %d requests in all, want 3", f.idp.count())
	}
}

func TestServiceGrant_FailureVisibleBeforeTokenExpires(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.mustToken(serviceGrantTriggerRefresh)
	f.idp.respond = func(int, svcReq) svcAnswer { return svcOAuthError(401, "invalid_client") }
	f.clock.Advance(51 * time.Minute) // the cached token has 9 minutes left; the refresher is due
	f.srv.serviceGrantPass(context.Background())
	if f.idp.count() != 2 {
		t.Fatalf("requests = %d, want the pass to try a new token", f.idp.count())
	}
	rows, _ := f.st.ServiceGrantStatuses(context.Background(), []string{f.uid})
	if st := serviceGrantStatusOf(f.provider(), rows); st.State != serviceGrantStateFailed || st.Replicas[0].Reason != "invalid_client" {
		t.Fatalf("status = %+v, want failing while the old token still has life", st)
	}
	if rows[0].LastOKAt == nil || rows[0].ValidUntil == nil {
		t.Errorf("the failure erased the last success: %+v", rows[0])
	}
}

func TestServiceGrant_ConcurrentResolvesMintOnce(t *testing.T) {
	f := newSvcFixWithSecret(t)
	release := make(chan struct{})
	f.idp.respond = func(n int, _ svcReq) svcAnswer { a := svcOK(n, 3600); a.Block = release; return a }
	var wg sync.WaitGroup
	got := make([]string, 8)
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			tok, err := f.srv.serviceGrantToken(context.Background(), f.provider(), "", serviceGrantTriggerResolve)
			if err == nil {
				got[i] = tok.token
			}
		}()
	}
	for f.idp.count() == 0 {
		time.Sleep(time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()
	for i, tok := range got {
		if tok != svcToken(1) {
			t.Fatalf("caller %d got %q, want the one minted token", i, tok)
		}
	}
	if f.idp.count() != 1 || len(f.mintRows()) != 1 {
		t.Fatalf("requests = %d, mint audit rows = %d, want 1 and 1", f.idp.count(), len(f.mintRows()))
	}
	_ = f.mustToken(serviceGrantTriggerResolve) // a later cache hit
	if f.idp.count() != 1 || len(f.mintRows()) != 1 {
		t.Errorf("a cache hit sent a request or wrote a row: %d, %d", f.idp.count(), len(f.mintRows()))
	}
}

func TestServiceGrant_SecretAbsentFromLogsAndErrors(t *testing.T) {
	f := newSvcFixWithSecret(t)
	f.idp.respond = func(int, svcReq) svcAnswer {
		return svcAnswer{Status: 400, Body: `{"error":"invalid_client","error_description":"bad secret ` + svcClientSecret + `"}`}
	}
	ids := f.captureRowIDs()
	_, err := f.token(serviceGrantTriggerRefresh)
	if err == nil || strings.Contains(err.Error(), svcClientSecret) {
		t.Fatalf("error = %v", err)
	}
	f.awaitRows(t, ids()...) // the failure row settles after the answer: the sweep covers it and its copy
	f.awaitSink(t, ids()...)
	row, _ := f.st.statusOf(f.uid, f.srv.svcGrant.replica())
	haystack := f.logs.String() + f.auditAll() + err.Error() + row.Reason + row.OAuthError
	if strings.Contains(haystack, svcClientSecret) || strings.Contains(haystack, "bad secret") {
		t.Errorf("the secret or error_description reached a log, an audit row, an error or the status row")
	}
}

func TestServiceGrant_TokenCallUsesDaemonTransport(t *testing.T) {
	f := newSvcFixWithSecret(t)
	var mu sync.Mutex
	var connects []string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodConnect {
			connects = append(connects, r.Host)
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	t.Cleanup(proxy.Close)
	pu, _ := url.Parse(proxy.URL)
	f.srv.cfg.ServiceGrantTransport = &http.Transport{Proxy: http.ProxyURL(pu)}
	ids := f.captureRowIDs()
	if _, err := f.token(serviceGrantTriggerRefresh); err == nil {
		t.Fatal("the proxy refused the tunnel and the mint succeeded")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connects) != 1 || connects[0] != "idp.example.test:443" {
		t.Fatalf("proxy saw CONNECT %v, want one for the token host", connects)
	}
	if len(ids()) != 1 {
		t.Fatalf("row ids = %v, want the one request's", ids())
	}
	f.awaitRows(t, ids()[0]) // the failure row settles after the answer
	rows := f.st.rowsOf(ids()[0])
	if len(rows) != 1 || !strings.Contains(string(rows[0].Data), `"egress":"proxy"`) {
		t.Fatalf("audit rows = %v, want one naming egress proxy", rows)
	}
}
