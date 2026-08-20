// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// W31-S1-7: /me used to say nothing about when an SSO session would die, so
// the console had no way to warn ahead of the silent 401 the expiry causes.
// handleMe now includes session_expires_at when (and only when) a verified
// OIDC session is on the context.
func TestHandleMe_SessionExpiry(t *testing.T) {
	s := &Server{}

	t.Run("SSO session publishes session_expires_at", func(t *testing.T) {
		exp := time.Now().Add(45 * time.Minute).UTC().Truncate(time.Second)
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		ctx := withOIDCHuman(r.Context(), "sub-alice")
		ctx = withOIDCExpiry(ctx, exp)
		r = r.WithContext(ctx)

		w := httptest.NewRecorder()
		s.handleMe(w, r)

		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		got, ok := body["session_expires_at"].(string)
		if !ok {
			t.Fatalf("session_expires_at missing or not a string: %#v", body["session_expires_at"])
		}
		gotT, err := time.Parse(time.RFC3339, got)
		if err != nil {
			t.Fatalf("session_expires_at not RFC3339: %v", err)
		}
		if !gotT.Equal(exp) {
			t.Fatalf("session_expires_at = %v, want %v", gotT, exp)
		}
	})

	t.Run("no SSO session (admin token / local mode) omits the field", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
		w := httptest.NewRecorder()
		s.handleMe(w, r)

		var body map[string]any
		if err := json.NewDecoder(w.Body).Decode(&body); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if _, present := body["session_expires_at"]; present {
			t.Fatalf("session_expires_at present with no OIDC session: %#v", body["session_expires_at"])
		}
	})
}
