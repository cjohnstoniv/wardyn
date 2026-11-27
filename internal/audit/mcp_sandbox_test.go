// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"encoding/json"
	"testing"
)

// TestMCPSandboxStampOverridesPayload: an MCP proxy's row carries its server key as mcp_sandbox, over a value
// the payload named, keeping every other field; a row with no key in context is unchanged.
func TestMCPSandboxStampOverridesPayload(t *testing.T) {
	ctx := WithMCPSandbox(t.Context(), "files")
	if got := MCPSandboxOf(ctx); got != "files" {
		t.Fatalf("MCPSandboxOf = %q", got)
	}
	if got := MCPSandboxOf(t.Context()); got != "" {
		t.Fatalf("MCPSandboxOf without a key = %q", got)
	}
	for _, raw := range []string{``, `null`, `{"mcp_sandbox":"forged","host":"api.files.test"}`, `[1]`} {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(StampMCPSandbox(ctx, json.RawMessage(raw)), &data); err != nil {
			t.Fatal(err)
		}
		if string(data["mcp_sandbox"]) != `"files"` {
			t.Errorf("%s: mcp_sandbox = %s", raw, data["mcp_sandbox"])
		}
		if raw == `[1]` && string(data["data"]) != raw {
			t.Errorf("lost the original %s: %v", raw, data)
		}
		if raw == `{"mcp_sandbox":"forged","host":"api.files.test"}` && string(data["host"]) != `"api.files.test"` {
			t.Errorf("lost a field: %v", data)
		}
		if untouched := StampMCPSandbox(t.Context(), json.RawMessage(raw)); string(untouched) != raw {
			t.Errorf("a row with no key changed: %s", untouched)
		}
	}
}
