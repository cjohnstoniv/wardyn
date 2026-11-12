// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"
)

func TestGetRunSyncRules_ReadsTheRunsSyncDeny(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /api/v1/runs/"+id.String() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "sync": map[string]any{"open": []any{}, "deny": []string{".env*"}}})
	}))
	t.Cleanup(srv.Close)
	rules, err := newTestClient(srv).GetRunSyncRules(context.Background(), id)
	if err != nil || !slices.Equal(rules.Deny, []string{".env*"}) {
		t.Errorf("rules = %+v, %v", rules, err)
	}
}

func TestGetRunAllowedModels_ReadsTheRunsList(t *testing.T) {
	id := uuid.New()
	body := map[string]any{"id": id, "allowed_models": map[string]any{"models": []string{"model-a"}, "unit": map[string]any{"id": "p1", "name": "Contractors"}}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /api/v1/runs/"+id.String() {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, body)
	}))
	t.Cleanup(srv.Close)
	got, err := newTestClient(srv).GetRunAllowedModels(context.Background(), id)
	if err != nil || got == nil || !slices.Equal(got.Models, []string{"model-a"}) || got.Profile.Name != "Contractors" || got.Profile.ID != "p1" {
		t.Fatalf("allowed models = %+v, %v", got, err)
	}
	delete(body, "allowed_models")
	if got, err := newTestClient(srv).GetRunAllowedModels(context.Background(), id); err != nil || got != nil {
		t.Fatalf("a run with no limit = %+v, %v, want nil", got, err)
	}
}
