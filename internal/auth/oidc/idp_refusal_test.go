// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A provider that returns an error of its own (not assigned to the app, consent declined, a hiccup)
// sends the browser back to the console with a stable code, never a bare "missing code parameter".
func TestCallbackRedirectsAnIdentityProviderRefusal(t *testing.T) {
	_, auth := lifeAuth(t, nil)
	for idpErr, want := range map[string]string{
		"access_denied":           "oidc_refused",
		"unauthorized_client":     "oidc_refused",
		"temporarily_unavailable": "oidc_transient",
		"server_error":            "oidc_transient",
	} {
		r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s1&error="+idpErr, nil)
		for _, c := range []*http.Cookie{{Name: "wardyn_oidc_state", Value: "s1"}, {Name: "wardyn_oidc_nonce", Value: "n"}, {Name: "wardyn_oidc_pkce", Value: "p"}} {
			r.AddCookie(c)
		}
		w := httptest.NewRecorder()
		auth.CallbackHandler(w, r)
		if loc := w.Result().Header.Get("Location"); w.Code != http.StatusFound || loc != "/?auth_error="+want {
			t.Errorf("error=%s: %d %q, want a redirect to auth_error=%s", idpErr, w.Code, loc, want)
		}
	}
}
