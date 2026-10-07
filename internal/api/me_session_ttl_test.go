// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestMe_SessionExpiresAtFollowsTheCookie: /me's session_expires_at is the session cookie's own
// expiry, read through the real router, so a session WARDYN_OIDC_SESSION_TTL stretched past the ID
// token's lifetime reports its new end, not an hour.
func TestMe_SessionExpiresAtFollowsTheCookie(t *testing.T) {
	srv := rbacServer(t)
	exp := time.Now().UTC().Add(20 * time.Hour).Truncate(time.Second)
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: "sub-ttl", Email: "ttl@example.com", Role: oidc.RoleUser,
		UserType: types.UserTypeStandard, Expiry: exp, IssuedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	w := doSSO(t, srv, http.MethodGet, "/api/v1/me", signedSessionCookie(payload), "")
	if w.Code != http.StatusOK {
		t.Fatalf("/me status = %d, body=%s", w.Code, w.Body.String())
	}
	var body struct {
		SessionExpiresAt string `json:"session_expires_at"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	got, err := time.Parse(time.RFC3339, body.SessionExpiresAt)
	if err != nil || !got.Equal(exp) {
		t.Fatalf("session_expires_at = %q (%v), want %v", body.SessionExpiresAt, err, exp)
	}
}
