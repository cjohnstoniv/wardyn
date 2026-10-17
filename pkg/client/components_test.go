// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestComponents_MethodsHitTheirRoutes pins each method to its verb and path and
// the body it sends, and that the saved-row answer decodes flat (the row's own
// fields beside requirements).
func TestComponents_MethodsHitTheirRoutes(t *testing.T) {
	id := uuid.New()
	req := client.ComponentRequest{Name: "Stripe", Definition: client.ComponentDefinition{Hosts: []string{"api.example.com"}}}
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r.Method+" "+r.URL.Path)
		checkAuth(t, r)
		switch r.Method {
		case http.MethodPost, http.MethodPut:
			var body client.ComponentRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Name != "Stripe" || len(body.Definition.Hosts) != 1 {
				t.Errorf("%s %s body = %+v (%v), want the request", r.Method, r.URL.Path, body, err)
			}
			writeJSON(w, http.StatusOK, map[string]any{"id": id, "name": "Stripe", "version": 3, "requirements": []any{}})
		case http.MethodGet:
			if r.URL.Path == "/api/v1/components" {
				writeJSON(w, http.StatusOK, []client.Component{{ID: id, Name: "Stripe"}})
				return
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"may_define": true, "resident_delivery_allowed": false, "autonomy_cap": "L1",
				"mine": []any{}, "org": []any{map[string]any{"id": id, "name": "Org", "hosts": []string{"h.example.com"}, "secrets": []any{}, "config_keys": []string{"A"}}},
			})
		default:
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()
	c := newTestClient(srv)
	ctx := context.Background()

	mine, err := c.MyComponents(ctx)
	if err != nil || !mine.MayDefine || mine.ResidentDeliveryAllowed || mine.AutonomyCap != client.AutonomyL1 ||
		len(mine.Org) != 1 || mine.Org[0].ConfigKeys[0] != "A" {
		t.Fatalf("MyComponents = %+v, %v", mine, err)
	}
	for _, tc := range []struct {
		name string
		call func() (client.ComponentSaved, error)
	}{
		{"save", func() (client.ComponentSaved, error) { return c.SaveMyComponent(ctx, req) }},
		{"update", func() (client.ComponentSaved, error) { return c.UpdateMyComponent(ctx, id, req) }},
		{"put", func() (client.ComponentSaved, error) { return c.PutComponent(ctx, id, req) }},
	} {
		got, err := tc.call()
		if err != nil || got.ID != id || got.Version != 3 || got.Requirements == nil {
			t.Fatalf("%s = %+v, %v; want the row decoded flat with an empty requirements list", tc.name, got, err)
		}
	}
	if org, err := c.ListComponents(ctx); err != nil || len(org) != 1 || org[0].ID != id {
		t.Fatalf("ListComponents = %+v, %v", org, err)
	}
	if err := c.DeleteMyComponent(ctx, id); err != nil {
		t.Fatalf("DeleteMyComponent: %v", err)
	}
	if err := c.DeleteComponent(ctx, id); err != nil {
		t.Fatalf("DeleteComponent: %v", err)
	}

	want := []string{
		"GET /api/v1/me/components", "POST /api/v1/me/components", "PUT /api/v1/me/components/" + id.String(),
		"PUT /api/v1/components/" + id.String(), "GET /api/v1/components",
		"DELETE /api/v1/me/components/" + id.String(), "DELETE /api/v1/components/" + id.String(),
	}
	if !slices.Equal(seen, want) {
		t.Fatalf("requests = %v, want %v", seen, want)
	}
}
