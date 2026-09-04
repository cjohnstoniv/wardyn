// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestCallbackCookiesAreNotSpentOnAFailedCallback pins the ORDER inside
// CallbackHandler's first phase: every one-time cookie is read before ANY of
// them is cleared.
//
// The three are single-use by construction — state guards this redirect, nonce
// binds the ID token, the verifier proves the exchange — so clearing one before
// the others are known to be present would spend it on a request that never
// reaches the token exchange, turning a retryable error into a login the human
// cannot repeat by pressing back.
//
// It exists because that ordering was documented and enforced by nothing:
// moving `clearCookie(w, stateCookieName)` above the nonce read left the whole
// package green (executed). No test named these failure paths at all — grepping
// the package for "missing nonce cookie" returned no test.
//
// A zero Authenticator is deliberate: this phase reads only the request and the
// response writer, so the case needs no IdP, no discovery and no keys — which is
// itself part of why it can be a phase.
func TestCallbackCookiesAreNotSpentOnAFailedCallback(t *testing.T) {
	cleared := func(w *httptest.ResponseRecorder, name string) bool {
		for _, c := range w.Result().Cookies() {
			if c.Name == name && c.MaxAge == -1 {
				return true
			}
		}
		return false
	}

	for _, tc := range []struct {
		name    string
		cookies []*http.Cookie
		wantMsg string
	}{
		{
			// State matches, so the CSRF check passes and the handler goes on to
			// the nonce — which is absent. Nothing may be spent.
			name:    "nonce missing: the state cookie survives",
			cookies: []*http.Cookie{{Name: "wardyn_oidc_state", Value: "s1"}},
			wantMsg: "missing nonce cookie",
		},
		{
			// One further in: state and nonce both present, pkce absent.
			name: "pkce missing: state and nonce both survive",
			cookies: []*http.Cookie{
				{Name: "wardyn_oidc_state", Value: "s1"},
				{Name: "wardyn_oidc_nonce", Value: "n1"},
			},
			wantMsg: "missing pkce cookie",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&code=c1", nil)
			for _, c := range tc.cookies {
				r.AddCookie(c)
			}
			w := httptest.NewRecorder()
			(&writoidc.Authenticator{}).CallbackHandler(w, r)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if got := w.Body.String(); !strings.Contains(got, tc.wantMsg) {
				t.Fatalf("body = %q, want it to name %q — the fixture is not reaching the phase it claims to test", got, tc.wantMsg)
			}
			for _, c := range tc.cookies {
				if cleared(w, c.Name) {
					t.Errorf("%s was CLEARED on a failed callback: a single-use cookie was spent on a request that "+
						"never reached the token exchange, so the human cannot retry this login", c.Name)
				}
			}
		})
	}

	// The control: on the SUCCESS path of this phase all three ARE spent. Without
	// it, a handler that never cleared anything would pass every case above.
	t.Run("all three are spent once the phase succeeds", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&code=", nil)
		for _, c := range []*http.Cookie{
			{Name: "wardyn_oidc_state", Value: "s1"},
			{Name: "wardyn_oidc_nonce", Value: "n1"},
			{Name: "wardyn_oidc_pkce", Value: "v1"},
		} {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		(&writoidc.Authenticator{}).CallbackHandler(w, r)
		// An empty code stops the handler in the NEXT phase, after phase 1 has
		// already cleared — which is exactly the boundary being pinned.
		if !strings.Contains(w.Body.String(), "missing code parameter") {
			t.Fatalf("body = %q, want the handler to have passed phase 1 and stopped at the code check", w.Body.String())
		}
		for _, name := range []string{"wardyn_oidc_state", "wardyn_oidc_nonce", "wardyn_oidc_pkce"} {
			if !cleared(w, name) {
				t.Errorf("%s was NOT cleared after phase 1 succeeded; a single-use cookie outlived its one use", name)
			}
		}
	})
}
