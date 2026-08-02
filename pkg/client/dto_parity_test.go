// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// dto_parity_test.go pins the SDK request DTOs' wire tag OPTIONS.
//
// Type identity is guarded elsewhere: internal/api declares each server body as
// `type xRequest = client.XRequest` and pins those aliases at COMPILE time from
// inside the package (internal/api/dto_alias_test.go) — a re-expanded struct
// copy fails to build there, so no source parsing is needed from this side.
// What the alias identity does NOT cover is the json tag options on the SDK
// types; the test below pins those.

import (
	"encoding/json"
	"maps"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestRequestDTOs_ZeroValueOmitOptionals pins the tag OPTIONS the alias identity
// does not cover: a minimal request must put only the always-sent keys on the
// wire, so a new optional field added without omitempty (which would post e.g.
// "task_mode":"" or "llm_cred":null and override a server default or binding)
// fails here rather than in production.
func TestRequestDTOs_ZeroValueOmitOptionals(t *testing.T) {
	for _, tc := range []struct {
		name string
		req  any
		want []string
	}{
		{"CreateRunRequest", client.CreateRunRequest{}, []string{"agent", "repo"}},
		{"PolicyRequest", client.PolicyRequest{}, []string{"name", "spec"}},
		{"WorkspaceRequest", client.WorkspaceRequest{}, []string{"kind", "name", "source"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b, err := json.Marshal(tc.req)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var raw map[string]any
			if err := json.Unmarshal(b, &raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got := slices.Sorted(maps.Keys(raw))
			if !slices.Equal(got, tc.want) {
				t.Errorf("zero-value %s marshals to keys %v, want %v — every "+
					"optional field needs `omitempty` so a minimal request does not post "+
					"empty values the server would treat as explicit choices", tc.name, got, tc.want)
			}
		})
	}
}
