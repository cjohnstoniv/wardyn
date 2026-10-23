// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"
	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// memSessionSIDs is the sid -> person store in memory, keyed issuer + " " + sid.
type memSessionSIDs struct {
	mu   sync.Mutex
	rows map[string]string
}

func (m *memSessionSIDs) RecordOIDCSessionSID(_ context.Context, issuer, sid, sub string, _ time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[issuer+" "+sid] = sub
	return nil
}

func (m *memSessionSIDs) TakeOIDCSessionSID(_ context.Context, issuer, sid string) (string, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sub, ok := m.rows[issuer+" "+sid]
	delete(m.rows, issuer+" "+sid)
	return sub, ok, nil
}

// frontchannelAuthenticator builds an Authenticator over a test identity provider with front-channel
// logout on or off, returning it, the provider's issuer and the sid store.
func frontchannelAuthenticator(t *testing.T, on bool) (*oidc.Authenticator, string, *memSessionSIDs) {
	t.Helper()
	idp := &gooidctest.Server{}
	httpSrv := httptest.NewServer(idp)
	idp.SetIssuer(httpSrv.URL)
	t.Cleanup(httpSrv.Close)
	sids := &memSessionSIDs{rows: map[string]string{}}
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: httpSrv.URL, ClientID: "wardyn-client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
		Revocations: fakeAuthzSessionRevocations{}, FrontchannelLogout: on, SessionSIDs: sids,
	}, accessTestHMACKey)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	return auth, httpSrv.URL, sids
}

// frontchannelServer is a Server over frontchannelAuthenticator, also returning its audit rows.
func frontchannelServer(t *testing.T, on bool) (*Server, string, *memSessionSIDs, *memAudit) {
	t.Helper()
	auth, iss, sids := frontchannelAuthenticator(t, on)
	fixed, aud := time.Now(), &memAudit{}
	return New(Config{OIDC: auth, Audit: aud, Now: func() time.Time { return fixed }}), iss, sids, aud
}

func getFrontchannelRoute(t *testing.T, srv *Server, q url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/auth/frontchannel-logout?"+q.Encode(), nil))
	return w
}

// The front-channel logout route exists only when the operator turned it on.
func TestFrontchannelLogoutRouteMountedOnlyWhenOn(t *testing.T) {
	off, _, _, _ := frontchannelServer(t, false)
	if code := getFrontchannelRoute(t, off, nil).Code; code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("flag off: GET /auth/frontchannel-logout = %d, want 404 or 405", code)
	}
	on, _, _, _ := frontchannelServer(t, true)
	if code := getFrontchannelRoute(t, on, nil).Code; code != http.StatusBadRequest {
		t.Errorf("flag on: GET with no iss or sid = %d, want the handler's 400", code)
	}
}

// A front-channel cut writes the session.revoke row a back-channel cut writes, marked with its own source.
func TestFrontchannelLogoutWritesTheSessionRevokeRow(t *testing.T) {
	srv, iss, sids, aud := frontchannelServer(t, true)
	_ = sids.RecordOIDCSessionSID(context.Background(), iss, "idp-session-7", "pairwise-1", time.Now())
	if w := getFrontchannelRoute(t, srv, url.Values{"iss": {iss}, "sid": {"idp-session-7"}}); w.Code != http.StatusOK {
		t.Fatalf("logout = %d %s, want 200", w.Code, w.Body.String())
	}
	rows := aud.find("session.revoke")
	if len(rows) != 1 || rows[0].Target != "pairwise-1" || rows[0].Outcome != "success" {
		t.Fatalf("session.revoke rows = %+v, want one success for pairwise-1", rows)
	}
	var data map[string]any
	_ = json.Unmarshal(rows[0].Data, &data)
	if data["source"] != "idp_front_channel_logout" || data["sessions_only"] != true || data["sub"] != "pairwise-1" {
		t.Errorf("row data = %v, want source idp_front_channel_logout, sessions_only, sub pairwise-1", data)
	}
}

// Requests that name no sign-in (forged or stale sids) are refused with 429 past
// backchannelLogoutBurst; requests that cut give their charge back, so a run of them is never refused.
func TestFrontchannelLogoutBoundsRequestsThatCutNothing(t *testing.T) {
	srv, iss, sids, _ := frontchannelServer(t, true)
	for i := range 3 * int(backchannelLogoutBurst) {
		sid := fmt.Sprintf("idp-session-%d", i)
		_ = sids.RecordOIDCSessionSID(context.Background(), iss, sid, "pairwise-1", time.Now())
		if w := getFrontchannelRoute(t, srv, url.Values{"iss": {iss}, "sid": {sid}}); w.Code != http.StatusOK {
			t.Fatalf("genuine logout %d = %d, want 200", i, w.Code)
		}
	}
	answered, refused := 0, 0
	for i := range 20 {
		switch getFrontchannelRoute(t, srv, url.Values{"iss": {iss}, "sid": {fmt.Sprintf("forged-%d", i)}}).Code {
		case http.StatusOK:
			answered++
		case http.StatusTooManyRequests:
			refused++
		}
	}
	if answered > int(backchannelLogoutBurst) || refused == 0 {
		t.Errorf("20 forged sids: %d answered, %d refused; want at most %v answered before 429", answered, refused, backchannelLogoutBurst)
	}
}

// A census of the router with front-channel logout on: every route but the front-channel logout keeps
// frame-ancestors 'none' and X-Frame-Options DENY, and that one may be framed by the issuer's origin
// alone, on every answer it gives.
func TestOnlyTheFrontchannelLogoutMayBeFramedAndOnlyByTheIssuer(t *testing.T) {
	auth, iss, _ := frontchannelAuthenticator(t, true)
	srv, _, _, _ := newAuthzMatrixServer(t, func(c *Config) { c.OIDC = auth })
	param := regexp.MustCompile(`\{[^}]+\}`)
	n, framed := 0, 0
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		n++
		path := strings.ReplaceAll(param.ReplaceAllString(route, "x"), "/*", "/x")
		rec := httptest.NewRecorder()
		panicFails(t, srv.Handler()).ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		csp, xfo := rec.Header().Get("Content-Security-Policy"), rec.Header().Get("X-Frame-Options")
		if route == "/auth/frontchannel-logout" {
			framed++
			if !strings.Contains(csp, "frame-ancestors "+iss+";") || strings.Contains(csp, "frame-ancestors 'none'") || xfo != "" {
				t.Errorf("%s %s: CSP %q, X-Frame-Options %q; want frame-ancestors %s alone and no X-Frame-Options", method, route, csp, xfo, iss)
			}
			return nil
		}
		if !strings.Contains(csp, "frame-ancestors 'none'") || xfo != "DENY" {
			t.Errorf("%s %s: CSP %q, X-Frame-Options %q; want frame-ancestors 'none' and DENY", method, route, csp, xfo)
		}
		return nil
	})
	if err != nil || n < 50 || framed != 1 {
		t.Fatalf("chi.Walk: %v after %d routes, %d front-channel; want the whole router and the route once", err, n, framed)
	}
}
