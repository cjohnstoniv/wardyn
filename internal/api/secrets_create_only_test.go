// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

// TestSecretCreateOnlyNeedsIfNoneMatchStar: only If-None-Match: * makes a secret PUT create-only, so every client
// that sends no such header keeps the upsert it has always had.
func TestSecretCreateOnlyNeedsIfNoneMatchStar(t *testing.T) {
	for _, tc := range []struct {
		header  string
		guarded bool
	}{{"*", true}, {" * ", true}, {"", false}, {`"abc"`, false}, {"*, x", false}} {
		r := httptest.NewRequest(http.MethodPut, "/api/v1/secrets/npm-token", nil)
		if tc.header != "" {
			r.Header.Set("If-None-Match", tc.header)
		}
		rev, guarded := secretstore.IfRevisionFrom(secretCreateOnly(context.Background(), r))
		if guarded != tc.guarded || rev != "" {
			t.Errorf("If-None-Match %q: guarded=%v rev=%q, want guarded=%v rev=\"\"", tc.header, guarded, rev, tc.guarded)
		}
	}
}

// TestRefuseSecretExistsAnswers409: a create-only Put that found the name taken is a 409 with its own reason, not
// writeServerError's 500; any other failure is left to the caller's existing handling.
func TestRefuseSecretExistsAnswers409(t *testing.T) {
	w := httptest.NewRecorder()
	if !refuseSecretExists(w, fmt.Errorf("put: %w", secretstore.ErrRevisionChanged)) {
		t.Fatal("a wrapped ErrRevisionChanged was not refused")
	}
	if w.Code != http.StatusConflict || errorReason(w) != "secret_exists" {
		t.Errorf("got %d reason %q, want 409 secret_exists; body=%s", w.Code, errorReason(w), w.Body.String())
	}
	other := httptest.NewRecorder()
	if refuseSecretExists(other, errors.New("store down")) || other.Body.Len() != 0 {
		t.Errorf("another error was answered here: %d %s", other.Code, other.Body.String())
	}
}
