// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestHeldResponseSendsJSONNoSniff: the held create answer goes out as application/json with nosniff,
// whatever type the create set, so its bytes (which can quote the request) are never read as HTML.
func TestHeldResponseSendsJSONNoSniff(t *testing.T) {
	w := httptest.NewRecorder()
	held := &heldResponse{w: w}
	held.Header().Set("Content-Type", "text/html")
	held.WriteHeader(http.StatusUnprocessableEntity)
	_, _ = held.Write([]byte(`{"error":"<script>"}`))
	held.send()
	if got := w.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if w.Code != http.StatusUnprocessableEntity || w.Body.String() != `{"error":"<script>"}` {
		t.Errorf("sent %d %q, want the held 422 and body", w.Code, w.Body)
	}
}
