// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types_test

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestComponentRef_MCPHarnessesAbsentVersusEmpty: absent (all harnesses) and
// an explicit [] (none) are different on the wire, and each survives an
// encode and a decode.
func TestComponentRef_MCPHarnessesAbsentVersusEmpty(t *testing.T) {
	id := uuid.New()
	cases := []struct {
		name   string
		ref    types.ComponentRef
		golden string
		want   func([]string) bool
	}{
		{"absent", types.ComponentRef{ID: &id}, `{"id":"` + id.String() + `"}`, func(h []string) bool { return h == nil }},
		{"none", types.ComponentRef{ID: &id, MCPHarnesses: []string{}}, `{"id":"` + id.String() + `","mcp_harnesses":[]}`, func(h []string) bool { return h != nil && len(h) == 0 }},
		{"some", types.ComponentRef{ID: &id, MCPHarnesses: []string{"codex-cli"}}, `{"id":"` + id.String() + `","mcp_harnesses":["codex-cli"]}`, func(h []string) bool { return len(h) == 1 }},
	}
	for _, c := range cases {
		b, err := json.Marshal(c.ref)
		if err != nil || string(b) != c.golden {
			t.Errorf("%s: encoded %s (%v), want %s", c.name, b, err, c.golden)
		}
		var back types.ComponentRef
		if err := json.Unmarshal([]byte(c.golden), &back); err != nil || !c.want(back.MCPHarnesses) {
			t.Errorf("%s: decoded %#v (%v)", c.name, back.MCPHarnesses, err)
		}
	}
}
