// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// A token lifetime above the API token ceiling is refused, and the sentence says the ceiling.
func TestInvitationLifetime_TokenTTLCeilingIsNamed(t *testing.T) {
	w := httptest.NewRecorder()
	if _, ok := invitationLifetime(w, createInvitationRequest{TokenTTLSeconds: apiTokenMaxTTLSeconds + 1}); ok {
		t.Fatal("a token lifetime above the ceiling was accepted")
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "token_ttl_seconds: between 0 and a hundred years") {
		t.Errorf("refusal = %d %s, want 400 naming the 100 year ceiling", w.Code, w.Body.String())
	}
	if _, ok := invitationLifetime(httptest.NewRecorder(), createInvitationRequest{TokenTTLSeconds: apiTokenMaxTTLSeconds}); !ok {
		t.Error("a token lifetime at the ceiling was refused")
	}
}
