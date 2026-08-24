// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// pingStore is a minimal store.Store double whose only interesting method is
// Ping — everything else panics if called, which /readyz must never do.
type pingStore struct {
	store.Store
	err error
}

func (p *pingStore) Ping(context.Context) error { return p.err }

// TestReadyzPingsStore covers the actual logic /readyz adds over /healthz: a
// failing Store.Ping must produce 503, a healthy one 200. Without this test a
// change that dropped the Ping call (reverting to /healthz's old unconditional
// "ok") would still pass every other suite.
func TestReadyzPingsStore(t *testing.T) {
	t.Run("db reachable", func(t *testing.T) {
		srv := New(Config{Store: &pingStore{}})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if body["status"] != "ok" {
			t.Fatalf("status field = %v, want ok", body["status"])
		}
	})

	t.Run("db unreachable", func(t *testing.T) {
		srv := New(Config{Store: &pingStore{err: errors.New("dial tcp: connection refused")}})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503", w.Code)
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("bad JSON: %v", err)
		}
		if body["status"] != "error" {
			t.Fatalf("status field = %v, want error", body["status"])
		}
	})

	t.Run("no store configured stays ok", func(t *testing.T) {
		srv := New(Config{})
		w := httptest.NewRecorder()
		srv.Handler().ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})
}
