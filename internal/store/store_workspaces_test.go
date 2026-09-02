// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Pins the nullable-JSONB param helpers that now share jsonOrNull's body. What
// matters is not the marshalling — it is WHICH emptiness each column collapses
// to SQL NULL, because the helpers disagree on purpose: the workspace columns
// read nil and empty the same way, while a source's requirements map MUST
// round-trip "non-nil but empty" as '{}' (a scan that legitimately found
// nothing) distinctly from nil ("leave requirements untouched"). Folding the
// seven helpers into one body is only safe while that disagreement survives,
// so it is asserted here rather than left to the WARDYN_TEST_PG round trips.
package store

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestJSONParamHelpers_NullSemantics(t *testing.T) {
	t.Run("workspace domain lists collapse nil and empty to NULL", func(t *testing.T) {
		for _, param := range []func([]string) any{workspaceApprovedParam, workspaceDeniedParam} {
			if got := param(nil); got != nil {
				t.Errorf("nil domains: got %#v, want SQL NULL", got)
			}
			if got := param([]string{}); got != nil {
				t.Errorf("empty domains: got %#v, want SQL NULL", got)
			}
			got, ok := param([]string{"example.com"}).([]byte)
			if !ok || string(got) != `["example.com"]` {
				t.Errorf("populated domains: got %#v, want [\"example.com\"]", got)
			}
		}
	})

	t.Run("workspace pointers are NULL when unset", func(t *testing.T) {
		if got := workspaceLLMCredParam(nil); got != nil {
			t.Errorf("nil llm cred: got %#v, want SQL NULL", got)
		}
		got, ok := workspaceLLMCredParam(&types.WorkspaceLLMCred{IntegrationRef: "anthropic"}).([]byte)
		if !ok || string(got) != `{"integration_ref":"anthropic"}` {
			t.Errorf("set llm cred: got %#v", got)
		}
		if got := workspaceBaseImageParam(nil); got != nil {
			t.Errorf("nil base image: got %#v, want SQL NULL", got)
		}
		got, ok = workspaceBaseImageParam(&types.WorkspaceBaseImage{Kind: "recommended"}).([]byte)
		if !ok || len(got) == 0 {
			t.Errorf("set base image: got %#v, want marshalled bytes", got)
		}
	})

	t.Run("workspace requirements collapse nil and empty to NULL", func(t *testing.T) {
		if got := workspaceRequirementsParam(nil); got != nil {
			t.Errorf("nil requirements: got %#v, want SQL NULL", got)
		}
		if got := workspaceRequirementsParam(map[string]types.WorkspaceRequirement{}); got != nil {
			t.Errorf("empty requirements: got %#v, want SQL NULL", got)
		}
		got, ok := workspaceRequirementsParam(map[string]types.WorkspaceRequirement{
			"GITHUB_TOKEN": {Level: "required", Provenance: "operator_set"},
		}).([]byte)
		if !ok || len(got) == 0 {
			t.Errorf("populated requirements: got %#v, want marshalled bytes", got)
		}
	})

	// The one helper that does NOT collapse empty: an empty-but-non-nil map is
	// a completed scan that found nothing and must rebuild the scan_seeded
	// subset, so it marshals to '{}'. Only nil reads as "leave untouched".
	t.Run("source requirements keep nil and empty apart", func(t *testing.T) {
		if got := sourceRequirementsParam(nil); got != nil {
			t.Errorf("nil source requirements: got %q, want NULL", got)
		}
		if got := sourceRequirementsParam(map[string]types.WorkspaceRequirement{}); string(got) != "{}" {
			t.Errorf("empty source requirements: got %q, want {}", got)
		}
	})
}
