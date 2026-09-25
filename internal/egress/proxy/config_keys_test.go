// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"encoding"
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"
)

// configKeysDir holds the key set this sidecar's strict decoder accepts, for
// this tree (current.txt) and for the last release (<tag>.txt). internal/api's
// TestDispatchConfigLoadsOnPreviousProxy loads every config dispatch writes
// against the release's file, because operators pin the proxy image separately
// from wardynd. RELEASING.md step 1b rolls it forward at each release.
const configKeysDir = "testdata/config-keys/"

// TestConfigKeySet makes a new sidecar config key a reviewed change. An older
// proxy refuses a key it does not know (LoadConfigBytes, DisallowUnknownFields),
// so a new key must be omitempty and set only by the feature that needs it.
//
// Regenerate with: WARDYN_UPDATE_GOLDEN=1 go test ./internal/egress/proxy/ -run TestConfigKeySet
func TestConfigKeySet(t *testing.T) {
	got := strings.Join(configKeyPaths(), "\n") + "\n"
	path := configKeysDir + "current.txt"
	if os.Getenv("WARDYN_UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("the sidecar config key set changed. An older proxy refuses any new key, so make it omitempty "+
			"and set it only for runs that use it, then regenerate with WARDYN_UPDATE_GOLDEN=1 go test "+
			"./internal/egress/proxy/ -run TestConfigKeySet.\n--- %s ---\n%s\n--- this tree ---\n%s", path, want, got)
	}
}

// configKeyPaths lists every JSON key the sidecar's strict decoder accepts, as
// dotted paths — walking configWire, the exact type LoadConfigBytes decodes
// into (Config plus the legacy ado_grants key), not Config alone, so the
// golden set here matches what DisallowUnknownFields actually refuses. Slices
// add no segment; a map whose values are structs adds "*" for its keys. The
// decoder does not check keys under a map or under a type with its own
// unmarshaller, so those appear as leaves.
func configKeyPaths() []string {
	var out []string
	var walk func(t reflect.Type, prefix string)
	walk = func(t reflect.Type, prefix string) {
		for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array {
			t = t.Elem()
		}
		if t.Kind() == reflect.Map {
			walk(t.Elem(), prefix+"*.")
			return
		}
		pt := reflect.PointerTo(t)
		if t.Kind() != reflect.Struct ||
			pt.Implements(reflect.TypeFor[json.Unmarshaler]()) || pt.Implements(reflect.TypeFor[encoding.TextUnmarshaler]()) {
			return
		}
		for i := range t.NumField() {
			f := t.Field(i)
			name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
			if !f.IsExported() || name == "-" {
				continue
			}
			if f.Anonymous && name == "" {
				walk(f.Type, prefix)
				continue
			}
			if name == "" {
				name = f.Name
			}
			out = append(out, prefix+name)
			walk(f.Type, prefix+name+".")
		}
	}
	walk(reflect.TypeFor[configWire](), "")
	slices.Sort(out)
	return out
}
