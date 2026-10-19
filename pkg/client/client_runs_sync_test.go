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
