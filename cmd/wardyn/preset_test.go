// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	sdk "github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestPresetGetApplyRoundTrip: `wardyn preset set` PUTs each preset by
// name, and `wardyn preset get > f && wardyn preset set f` leaves every
// version where it was. The fake bumps a version only on a changed body, the
// server's own rule (PutLaunchPreset).
func TestPresetGetApplyRoundTrip(t *testing.T) {
	var mu sync.Mutex
	presets := map[string]sdk.Preset{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/presets":
			doc := sdk.PresetsDocument{Presets: []sdk.Preset{}}
			for _, name := range []string{"alpha", "beta"} {
				if p, ok := presets[name]; ok {
					doc.Presets = append(doc.Presets, p)
				}
			}
			_ = json.NewEncoder(w).Encode(doc)
		case r.Method == http.MethodPut && strings.HasPrefix(r.URL.Path, "/api/v1/presets/"):
			var req sdk.PresetRequest
			dec := json.NewDecoder(r.Body)
			dec.DisallowUnknownFields()
			if err := dec.Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			name := strings.TrimPrefix(r.URL.Path, "/api/v1/presets/")
			p := presets[name]
			if p.Version == 0 || !reflect.DeepEqual(sdk.PresetRequest{Description: p.Description, UserTypes: p.UserTypes, Request: p.Request}, req) {
				p = sdk.Preset{Name: name, Version: p.Version + 1, Description: req.Description, UserTypes: req.UserTypes, Request: req.Request}
				presets[name] = p
			}
			_ = json.NewEncoder(w).Encode(p)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	run := func(args ...string) string {
		t.Helper()
		root := rootCmd()
		out := &strings.Builder{}
		root.SetArgs(append(args, "--url", srv.URL, "--token", "tok"))
		root.SetOut(out)
		root.SetErr(&strings.Builder{})
		if err := root.Execute(); err != nil {
			t.Fatalf("wardyn %v: %v", args, err)
		}
		return out.String()
	}
	path := filepath.Join(t.TempDir(), "presets.json")
	seed := `{"presets":[{"name":"alpha","user_types":["contractor"],"request":{"agent":"claude-code","repo":"acme/widgets"}},` +
		`{"name":"beta","description":"nightly","request":{"agent":"codex-cli","image":"ghcr.io/acme/agent:1"}}]}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	run("preset", "set", path)

	got := run("preset", "get")
	if err := os.WriteFile(path, []byte(got), 0o600); err != nil {
		t.Fatal(err)
	}
	run("preset", "set", path)
	var before, after sdk.PresetsDocument
	if err := json.Unmarshal([]byte(got), &before); err != nil {
		t.Fatalf("get printed %q: %v", got, err)
	}
	if err := json.Unmarshal([]byte(run("preset", "get")), &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Presets) != 2 || !reflect.DeepEqual(before, after) {
		t.Fatalf("get > f && apply f changed the presets:\nbefore %+v\nafter  %+v", before, after)
	}
	if after.Presets[0].Version != 1 || after.Presets[1].Request.Image != "ghcr.io/acme/agent:1" {
		t.Errorf("presets = %+v, want version 1 and the applied request", after.Presets)
	}
}

// A lone valid document with surrounding whitespace still applies.
func TestPresetSet_SingleDocumentWithTrailingNewlineApplies(t *testing.T) {
	var puts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			puts++
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"presets":[]}`))
	}))
	t.Cleanup(srv.Close)
	if _, err := operatorCommand(t, srv.URL, "\n"+`{"presets":[{"name":"new","request":{"agent":"claude-code"}}]}`+"\n\n", "preset", "set", "-"); err != nil {
		t.Fatalf("a single document with surrounding whitespace: %v", err)
	}
	if puts == 0 {
		t.Error("no write was issued for a valid single document")
	}
}
