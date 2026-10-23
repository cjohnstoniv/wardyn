// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// memSIDs is the sid -> person store in memory, keyed issuer + " " + sid.
type memSIDs map[string]string

func (m memSIDs) RecordOIDCSessionSID(_ context.Context, issuer, sid, sub string, _ time.Time) error {
	m[issuer+" "+sid] = sub
	return nil
}

func (m memSIDs) TakeOIDCSessionSID(_ context.Context, issuer, sid string) (string, bool, error) {
	sub, ok := m[issuer+" "+sid]
	delete(m, issuer+" "+sid)
	return sub, ok, nil
}

func frontchannelAuth(t *testing.T, env *idpEnv, rev writoidc.SessionRevocations, sids writoidc.SessionSIDs, on bool) *writoidc.Authenticator {
	t.Helper()
	return env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
		cfg.Revocations, cfg.SessionSIDs, cfg.FrontchannelLogout = rev, sids, on
	})
}

// signedInWithSID signs pairwise-1 in with an ID token whose sid claim is sid.
func signedInWithSID(t *testing.T, env *idpEnv, auth *writoidc.Authenticator, sid string) *http.Cookie {
	t.Helper()
	env.buildIDTokenRawClaim(t, "pairwise-1", "pat@corp.example", "sid", sid)
	w, _ := doCallbackVia(t, auth, auth.CallbackHandler)
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return c
		}
	}
	t.Fatalf("sign-in issued no session: %q", w.Result().Header.Get("Location"))
	return nil
}

func getFrontchannel(h http.HandlerFunc, q url.Values) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest(http.MethodGet, "/auth/frontchannel-logout?"+q.Encode(), nil))
	return w
}

// The provider's front-channel request, naming its issuer and the sid the sign-in carried, ends the
// person's console sessions with no cookie on the request: a cookie taken before is refused after,
// the answer is not cacheable, and the sid row is consumed.
func TestFrontchannelLogoutCutsTheSessionBySid(t *testing.T) {
	env := newIdPEnv(t)
	rev, sids := &cutLedger{seq: 10}, memSIDs{}
	auth := frontchannelAuth(t, env, rev, sids, true)
	cookie := signedInWithSID(t, env, auth, "idp-session-7")
	if live, reason := replay(auth, cookie); !live {
		t.Fatalf("a fresh session was refused: %q", reason)
	}
	var heard []string
	h := auth.FrontchannelLogoutHandler(func(_ *http.Request, sub string, _ error) { heard = append(heard, sub) })

	w := getFrontchannel(h, url.Values{"iss": {env.httpSrv.URL}, "sid": {"idp-session-7"}})
	if w.Code != http.StatusOK || !strings.Contains(w.Header().Get("Cache-Control"), "no-store") {
		t.Fatalf("logout = %d %s (Cache-Control %q), want 200 no-store", w.Code, w.Body.String(), w.Header().Get("Cache-Control"))
	}
	if live, reason := replay(auth, cookie); live || reason != "revoked_session" {
		t.Errorf("a cookie from before the logout = live %v, %q; want it revoked", live, reason)
	}
	if rev.cutCalls != 1 || rev.revokedSub != 0 || len(heard) != 1 || heard[0] != "pairwise-1" || len(sids) != 0 {
		t.Errorf("cuts %d, tokens-and-keys revokes %d, audit heard %v, sid rows left %v; want one session cut for pairwise-1 and the row consumed", rev.cutCalls, rev.revokedSub, heard, sids)
	}
}

// Only this client's issuer with a sid it recorded cuts anyone: a foreign or missing iss, and a
// missing sid, are refused with 400 and end nothing, leaving the row in place.
func TestFrontchannelLogoutRefusesAForeignIssuerOrAMissingSid(t *testing.T) {
	env := newIdPEnv(t)
	rev, sids := &cutLedger{seq: 10}, memSIDs{}
	auth := frontchannelAuth(t, env, rev, sids, true)
	signedInWithSID(t, env, auth, "idp-session-7")
	h := auth.FrontchannelLogoutHandler(nil)
	for name, q := range map[string]url.Values{
		"another issuer": {"iss": {"https://evil.example"}, "sid": {"idp-session-7"}},
		"no iss":         {"sid": {"idp-session-7"}},
		"no sid":         {"iss": {env.httpSrv.URL}},
		"neither":        {},
	} {
		if w := getFrontchannel(h, q); w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "invalid_request") {
			t.Errorf("%s = %d %s, want 400 invalid_request", name, w.Code, w.Body.String())
		}
	}
	if rev.cutCalls != 0 || len(sids) != 1 {
		t.Errorf("refused requests cut %d times and left sid rows %v; want no cut and the row kept", rev.cutCalls, sids)
	}
}

// A sid the sign-in did not carry is a no-op: 200, no cut, the session lives and its own row stays.
func TestFrontchannelLogoutWithAMismatchedSidEndsNothing(t *testing.T) {
	env := newIdPEnv(t)
	rev, sids := &cutLedger{seq: 10}, memSIDs{}
	auth := frontchannelAuth(t, env, rev, sids, true)
	cookie := signedInWithSID(t, env, auth, "idp-session-7")
	w := getFrontchannel(auth.FrontchannelLogoutHandler(nil), url.Values{"iss": {env.httpSrv.URL}, "sid": {"idp-session-8"}})
	if w.Code != http.StatusOK || rev.cutCalls != 0 || len(sids) != 1 {
		t.Errorf("mismatched sid = %d, cuts %d, sid rows %v; want 200, no cut and the row kept", w.Code, rev.cutCalls, sids)
	}
	if live, reason := replay(auth, cookie); !live {
		t.Errorf("the session ended on a mismatched sid: %q", reason)
	}
}

// Off (the default), a sign-in records no sid and the route is reported off.
func TestFrontchannelLogoutIsOffByDefault(t *testing.T) {
	env := newIdPEnv(t)
	sids := memSIDs{}
	off := frontchannelAuth(t, env, &cutLedger{}, sids, false)
	signedInWithSID(t, env, off, "idp-session-7")
	if off.FrontchannelLogoutEnabled() || len(sids) != 0 {
		t.Errorf("off: enabled %v, sid rows %v; want the route off and nothing recorded", off.FrontchannelLogoutEnabled(), sids)
	}
	if !frontchannelAuth(t, env, &cutLedger{}, sids, true).FrontchannelLogoutEnabled() {
		t.Error("FrontchannelLogout set but the route is reported off")
	}
}
