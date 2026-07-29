// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// dto_parity_test.go pins the SDK create-run DTO's wire tag OPTIONS.
//
// Type identity is guarded elsewhere: internal/api declares
// `type createRunRequest = client.CreateRunRequest` and pins that alias at
// COMPILE time from inside the package (internal/api/dto_alias_test.go) — a
// re-expanded struct copy fails to build there, so no source parsing is needed
// from this side. What the alias identity does NOT cover is the json tag
// options on the SDK type; the test below pins those.

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestCreateRunRequest_ZeroValueOmitsOptionals pins the tag OPTIONS the alias
// identity does not cover: a minimal request must put only the two always-sent
// keys on the wire, so a new optional field added without omitempty (which would
// post e.g. "task_mode":"" and override a server default) fails here rather than
// in production.
func TestCreateRunRequest_ZeroValueOmitsOptionals(t *testing.T) {
	b, err := json.Marshal(client.CreateRunRequest{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := slices.Sorted(maps.Keys(raw))
	want := []string{"agent", "repo"}
	if !slices.Equal(got, want) {
		t.Errorf("zero-value CreateRunRequest marshals to keys %v, want %v — every "+
			"optional field needs `omitempty` so a minimal run does not post empty "+
			"values the server would treat as explicit choices", got, want)
	}
}
