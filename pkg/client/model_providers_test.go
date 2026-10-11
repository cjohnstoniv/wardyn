// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestGetModelProviders_DecodesRecordsAndCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/model-providers" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"providers": []map[string]any{
				{"id": "anthropic", "uid": "u1", "kind": "anthropic"},
				{"id": "corp-gateway", "uid": "u2", "kind": "custom_endpoint", "base_url": "https://llm.corp.example"},
			},
			"connected_people": map[string]int{"anthropic": 3, "corp-gateway": 0},
		})
	}))
	t.Cleanup(srv.Close)
	got, err := newTestClient(srv).GetModelProviders(context.Background())
	if err != nil {
		t.Fatalf("GetModelProviders: %v", err)
	}
	if len(got.Providers) != 2 || got.Providers[1].ID != "corp-gateway" || got.Providers[1].BaseURL != "https://llm.corp.example" {
		t.Fatalf("providers = %+v, want both records decoded", got.Providers)
	}
	if got.ConnectedPeople["anthropic"] != 3 || got.ConnectedPeople["corp-gateway"] != 0 || len(got.ConnectedPeople) != 2 {
		t.Fatalf("connected_people = %v, want the per-provider counts", got.ConnectedPeople)
	}
}

// TestModelProvidersRead_HasNoSecretBearingField: GetModelProviders must never be able to return a
// credential, so no field reachable from its answer carries a name that says it holds one. A new
// field named like a secret fails here and has to be argued for.
func TestModelProvidersRead_HasNoSecretBearingField(t *testing.T) {
	var walk func(path string, ty reflect.Type, seen map[reflect.Type]bool)
	walk = func(path string, ty reflect.Type, seen map[reflect.Type]bool) {
		for ty.Kind() == reflect.Pointer || ty.Kind() == reflect.Slice || ty.Kind() == reflect.Map {
			ty = ty.Elem()
		}
		if ty.Kind() != reflect.Struct || seen[ty] {
			return
		}
		seen[ty] = true
		for i := 0; i < ty.NumField(); i++ {
			f := ty.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if name == "" || name == "-" {
				name = strings.ToLower(f.Name)
			}
			for _, bad := range []string{"secret", "token", "password", "key"} {
				if strings.Contains(name, bad) {
					t.Errorf("%s.%s (json %q) looks like it carries a credential: GetModelProviders must return configuration and counts only", path, f.Name, name)
				}
			}
			walk(path+"."+f.Name, f.Type, seen)
		}
	}
	walk("ModelProvidersRead", reflect.TypeOf(client.ModelProvidersRead{}), map[reflect.Type]bool{})
}
