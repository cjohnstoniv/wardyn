// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// TestUploadComposeResult_RoundTrip: a PUT under the run's OWN id is accepted
// (204) and the raw body is stashed in the compose-results store for the waiting
// RunClaudeCompose to take once (delete-on-read).
func TestUploadComposeResult_RoundTrip(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)
	body := `{"structured_output":{"run":{"agent":"claude-code"}}}`

	w := do(t, h.srv, http.MethodPut,
		"/api/v1/internal/compose-results/"+runID.String(), tok, body)
	if w.Code != http.StatusNoContent {
		t.Fatalf("compose upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	got, ok := h.srv.composeResults.take(runID)
	if !ok {
		t.Fatal("compose result was not stored for the run")
	}
	if string(got) != body {
		t.Errorf("stored body = %q, want %q", got, body)
	}
	// take() is delete-on-read: a second take must miss.
	if _, ok := h.srv.composeResults.take(runID); ok {
		t.Error("compose result should be taken exactly once (delete-on-read)")
	}
}

// TestUploadComposeResult_CrossRunRejected mirrors the scan/recording cross-run
// guard: a token minted for run A cannot PUT a proposal under run B's id (403),
// before any body read.
func TestUploadComposeResult_CrossRunRejected(t *testing.T) {
	h := newHarness(t)
	tok := h.mintRunToken(t, uuid.New())
	other := uuid.New()
	w := do(t, h.srv, http.MethodPut,
		"/api/v1/internal/compose-results/"+other.String(), tok, `{"x":1}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-run compose upload: code = %d, want 403", w.Code)
	}
	if _, ok := h.srv.composeResults.take(other); ok {
		t.Error("a rejected cross-run upload must store nothing")
	}
}
