// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestCallbackSessionTTL: unset, a console session ends at the ID token's expiry; set, it lasts
// SessionTTL from sign-in, also when the ID token expires sooner. The expiry is read back through
// Middleware, the value /me publishes as session_expires_at.
func TestCallbackSessionTTL(t *testing.T) {
	idTokenExpiry := time.Now().Add(20 * time.Minute).Truncate(time.Second)
	for _, tc := range []struct {
		name string
		ttl  time.Duration
	}{
		{"unset keeps the ID token's expiry", 0},
		{"set lasts the TTL past a sooner ID token expiry", 8 * time.Hour},
		{"set shorter than the ID token", 5 * time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newAuth(t, nil)
			writoidc.SetSessionTTLForTest(auth, tc.ttl)
			env.buildIDToken(t, "sub-ttl", "ttl@example.com", roleCallbackNonce, idTokenExpiry)

			before := time.Now()
			w := callbackOnce(t, auth)
			after := time.Now()
			if w.Code != http.StatusFound {
				t.Fatalf("callback status = %d, body=%q", w.Code, w.Body.String())
			}
			got := sessionExpiry(t, auth, w)
			if tc.ttl == 0 {
				if !got.Equal(idTokenExpiry) {
					t.Fatalf("session expiry = %v, want the ID token's %v", got, idTokenExpiry)
				}
				return
			}
			// The cookie codec may keep whole seconds only.
			lo, hi := before.Add(tc.ttl).Truncate(time.Second), after.Add(tc.ttl)
			if got.Before(lo) || got.After(hi) {
				t.Fatalf("session expiry = %v, want sign-in + %s (between %v and %v)", got, tc.ttl, lo, hi)
			}
		})
	}
}

// callbackOnce drives CallbackHandler with a fixed state, nonce and verifier, like doCallback.
func callbackOnce(t *testing.T, auth *writoidc.Authenticator) *httptest.ResponseRecorder {
	t.Helper()
	const stateVal = "state-ttl"
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state="+stateVal+"&code=testcode", nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: stateVal})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: roleCallbackNonce})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: "verifier-ttl"})
	w := httptest.NewRecorder()
	auth.CallbackHandler(w, r)
	return w
}

// sessionExpiry round-trips the issued session cookie through Middleware and returns
// ExpiryFromContext.
func sessionExpiry(t *testing.T, auth *writoidc.Authenticator, w *httptest.ResponseRecorder) time.Time {
	t.Helper()
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("no session cookie issued")
	}
	var got time.Time
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got = writoidc.ExpiryFromContext(r.Context())
	})
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.AddCookie(cookie)
	auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)
	if got.IsZero() {
		t.Fatal("Middleware did not accept the issued session")
	}
	return got
}
