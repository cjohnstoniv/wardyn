// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestWallPresets_WireShape(t *testing.T) {
	const id = "0b9f3c5e-2a51-4d39-8a3e-2f3c1f6e9a10"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		switch r.Method + " " + r.URL.Path {
		case "GET /api/v1/wall-presets":
			writeJSON(w, http.StatusOK, client.WallPresetList{Presets: []client.WallPreset{{ID: "sealed", Name: "Sealed"}}})
		case "GET /api/v1/wall-presets/read-only":
			writeJSON(w, http.StatusOK, client.WallPreset{ID: "read-only", Name: "Read-only"})
		case "POST /api/v1/wall-presets", "PUT /api/v1/wall-presets/" + id:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("body not JSON: %v", err)
			}
			if _, put := body["expected_version"]; put != (r.Method == http.MethodPut) || body["spec"] == nil {
				t.Errorf("%s body = %v", r.Method, body)
			}
			writeJSON(w, http.StatusOK, client.WallPreset{ID: id, Name: "Tight", Version: 1})
		case "DELETE /api/v1/wall-presets/" + id:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()
	c, ctx := newTestClient(srv), context.Background()

	if l, err := c.ListWallPresets(ctx); err != nil || len(l.Presets) != 1 || l.Presets[0].ID != "sealed" {
		t.Fatalf("ListWallPresets = %+v, %v", l, err)
	}
	if p, err := c.GetWallPreset(ctx, "read-only"); err != nil || p.Name != "Read-only" {
		t.Fatalf("GetWallPreset = %+v, %v", p, err)
	}
	req := client.WallPresetSaveRequest{Scope: "person", Name: "Tight"}
	if _, err := c.SaveWallPreset(ctx, "", req); err != nil {
		t.Fatalf("create: %v", err)
	}
	v := 1
	req.Scope, req.ExpectedVersion = "", &v
	if _, err := c.SaveWallPreset(ctx, id, req); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := c.DeleteWallPreset(ctx, id); err != nil {
		t.Fatalf("DeleteWallPreset: %v", err)
	}
}
