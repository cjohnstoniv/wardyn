// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"net/http"
	"strings"
	"testing"
)

// TestCallback_TokenResponseWithoutIDTokenIs401: an access token alone proves
// nothing about who signed in, so a token response with no id_token is refused
// and mints no session.
func TestCallback_TokenResponseWithoutIDTokenIs401(t *testing.T) {
	e := newEntraLogin(t, false)
	e.fake.SetOmitIDToken(true)
	w := e.run(t)
	// The body names the arm: without it the empty token would still fail
	// verification, and this test would pass on the wrong refusal.
	if w.Code != http.StatusUnauthorized || !strings.Contains(w.Body.String(), "id_token absent") {
		t.Fatalf("status = %d body=%q, want 401 naming the absent id_token", w.Code, w.Body.String())
	}
	if hasSession(w) {
		t.Fatal("a token response without an id_token minted a session")
	}
}
