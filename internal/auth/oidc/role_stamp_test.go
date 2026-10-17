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

// TestMiddlewareRoleStampTTL: with RoleStampTTL set, a session whose IssuedAt is older than the
// TTL is rejected as role_stamp_stale even though its Expiry is still in the future, and so is a
// cookie with no IssuedAt. With the TTL unset the same cookies authenticate.
func TestMiddlewareRoleStampTTL(t *testing.T) {
	for _, tc := range []struct {
		name     string
		ttl      time.Duration
		issuedAt time.Time
		reason   string
	}{
		{"off, old session", 0, time.Now().Add(-48 * time.Hour), ""},
		{"off, no iat", 0, time.Time{}, ""},
		{"fresh", time.Hour, time.Now().Add(-time.Minute), ""},
		{"older than the TTL", time.Hour, time.Now().Add(-2 * time.Hour), writoidc.SessionRoleStampStale},
		{"no iat", time.Hour, time.Time{}, writoidc.SessionRoleStampStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newAuth(t, nil)
			writoidc.SetRoleStampTTLForTest(auth, tc.ttl)
			cookie, err := writoidc.EncodeSessionForTest(auth, writoidc.Session{
				Sub: "sub-x", Email: "x@example.com", Role: writoidc.RoleAdmin, UserType: "standard",
				Expiry: time.Now().Add(time.Hour), IssuedAt: tc.issuedAt,
			})
			if err != nil {
				t.Fatalf("EncodeSessionForTest: %v", err)
			}
			var principalSet bool
			var got string
			next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				principalSet = writoidc.PrincipalFromContext(r.Context()) != ""
				got = writoidc.SessionRejectedFromContext(r.Context())
			})
			r := httptest.NewRequest(http.MethodGet, "/", nil)
			r.AddCookie(cookie)
			auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), r)
			if got != tc.reason {
				t.Errorf("SessionRejectedFromContext = %q, want %q", got, tc.reason)
			}
			if principalSet != (tc.reason == "") {
				t.Errorf("principal set = %v for rejection %q", principalSet, tc.reason)
			}
		})
	}
}
