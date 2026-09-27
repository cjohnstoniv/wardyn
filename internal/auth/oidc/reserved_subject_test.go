// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"net/http"
	"slices"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestCallbackRefusesReservedSubject: a subject the reserved func names is
// refused before any derivation — no session, the pre-existing cookie
// cleared, the generic auth_error, one DenialReservedPrincipal report, and
// OnLogin never called — while any other subject signs in through the same
// handler.
func TestCallbackRefusesReservedSubject(t *testing.T) {
	reserved := func(sub string) bool { return sub == "admin-token" }
	for _, c := range []struct {
		sub     string
		refused bool
	}{{"admin-token", true}, {"sub-person", false}} {
		t.Run(c.sub, func(t *testing.T) {
			env := newIdPEnv(t)
			var logins []string
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
				cfg.OnLogin = func(_ context.Context, sub, _, _ string, _ []string, _ bool) { logins = append(logins, sub) }
			})
			var reported []string
			env.buildIDTokenWithRoles(t, c.sub, "x@corp.example", nil, nil, roleCallbackNonce, time.Now().Add(time.Hour))
			w, sess := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(reserved, func(_ *http.Request, reason string) {
				reported = append(reported, reason)
			}))
			if !c.refused {
				if sess.Sub != c.sub || len(reported) != 0 || !slices.Equal(logins, []string{c.sub}) {
					t.Fatalf("control: session sub %q, reported %v, logins %v; want a normal sign-in", sess.Sub, reported, logins)
				}
				return
			}
			if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, "sign_in_refused") {
				t.Errorf("Location = %q, want the generic auth_error=sign_in_refused", loc)
			}
			if sess.Sub != "" {
				t.Errorf("a session was issued for reserved subject %q", sess.Sub)
			}
			assertSessionCookieCleared(t, w)
			if !slices.Equal(reported, []string{writoidc.DenialReservedPrincipal}) {
				t.Errorf("reported = %v, want [%s]", reported, writoidc.DenialReservedPrincipal)
			}
			if len(logins) != 0 {
				t.Errorf("OnLogin ran for a refused sign-in: %v", logins)
			}
		})
	}
}
