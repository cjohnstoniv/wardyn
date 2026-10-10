// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The console reads this same golden through both dry-run clients. The Go
// assertions compare actual door responses decoded by the SDK carrier.
func componentFactGolden(t *testing.T, key string, got client.ComponentFact) {
	t.Helper()
	raw, err := os.ReadFile("testdata/new_run_component_facts.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden map[string]json.RawMessage
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	want, exists := golden[key]
	if !exists {
		t.Fatalf("missing golden %q", key)
	}
	actual, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var actualJSON, wantJSON any
	if err := json.Unmarshal(actual, &actualJSON); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(want, &wantJSON); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(actualJSON, wantJSON) {
		t.Fatalf("%s actual = %s\nwant = %s", key, actual, want)
	}
}
