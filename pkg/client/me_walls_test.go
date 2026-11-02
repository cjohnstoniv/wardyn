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

func TestMyWalls_WireShape(t *testing.T) {
	list := []string{"pypi.org"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		if r.URL.Path != "/api/v1/me/walls" {
			t.Errorf("path = %s", r.URL.Path)
		}
		switch r.Method {
		case http.MethodGet:
			writeJSON(w, http.StatusOK, client.PersonalWalls{Spec: client.PersonalWallsSpec{AllowedDomains: &list}})
		case http.MethodPut:
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("body not JSON: %v", err)
			}
			if len(body) != 1 || body["spec"] == nil {
				t.Errorf("body = %v, want only {spec}", body)
			}
			writeJSON(w, http.StatusOK, client.PersonalWalls{Spec: client.PersonalWallsSpec{AllowedDomains: &list}})
		case http.MethodDelete:
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("method = %s", r.Method)
		}
	}))
	defer srv.Close()
	c, ctx := newTestClient(srv), context.Background()

	got, err := c.GetMyWalls(ctx)
	if err != nil || got.Spec.AllowedDomains == nil || (*got.Spec.AllowedDomains)[0] != "pypi.org" {
		t.Fatalf("GetMyWalls = %+v, %v", got, err)
	}
	if _, err := c.PutMyWalls(ctx, client.PersonalWallsSpec{AllowedDomains: &list}); err != nil {
		t.Fatalf("PutMyWalls: %v", err)
	}
	if err := c.ClearMyWalls(ctx); err != nil {
		t.Fatalf("ClearMyWalls: %v", err)
	}
}

// False turns open egress off for the run (a narrow); true asks for more (a set).
func TestOverrideItems_AllowAllEgressFalseIsANarrow(t *testing.T) {
	for _, tc := range []struct {
		v  bool
		op client.OverrideOp
	}{{true, client.OverrideSet}, {false, client.OverrideNarrow}} {
		items := client.RunOverrides{Egress: &client.RunEgressOverrides{AllowAllEgress: &tc.v}}.Items()
		if len(items) != 1 || items[0].Kind != client.OverrideAllowAllEgress || items[0].Op != tc.op {
			t.Errorf("allow_all_egress=%v: Items() = %+v, want one %s", tc.v, items, tc.op)
		}
	}
}
