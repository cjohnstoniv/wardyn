// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestRequestImageWire: RequestImage posts kind image with the ref, the reason and the ticket, and nothing
// else, and returns the request from the 202; the list reads image_offered.
func TestRequestImageWire(t *testing.T) {
	const ref = "registry.example.com/team/tool@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	unit, change := uuid.New(), uuid.New()
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/governance/requests" {
			t.Errorf("request %s %s, want /api/v1/governance/requests", r.Method, r.URL.Path)
		}
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, map[string]any{"requests": []any{}, "offered": []any{}, "image_offered": true})
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		writeJSON(w, http.StatusAccepted, map[string]any{
			"pending_change": map[string]any{"id": change, "state": "pending"},
			"request": map[string]any{"id": change, "state": "pending", "kind": "image", "unit_id": unit, "unit_name": "platform",
				"image": ref, "mine": true, "decidable": false, "deciders": []map[string]any{{"kind": "security"}}},
		})
	}))
	t.Cleanup(srv.Close)
	c := newTestClient(srv)
	req, err := c.RequestImage(context.Background(), ref, "the build needs it", "CHG-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 || got["kind"] != "image" || got["image"] != ref || got["reason"] != "the build needs it" || got["ticket"] != "CHG-1" {
		t.Errorf("body = %v, want exactly kind, image, reason and ticket", got)
	}
	if req.ID != change || req.Kind != "image" || req.Image != ref || req.UnitID == nil || *req.UnitID != unit || !req.Mine {
		t.Errorf("request = %+v, want the 202's request", req)
	}
	got = nil
	if _, err := c.RequestImage(context.Background(), ref, "again", ""); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["ticket"]; ok || len(got) != 3 {
		t.Errorf("body without a ticket = %v, want kind, image and reason only", got)
	}
	l, err := c.ListGovernanceRequests(context.Background())
	if err != nil || !l.ImageOffered {
		t.Errorf("list = %+v, %v; want image_offered true", l, err)
	}
}
