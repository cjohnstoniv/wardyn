// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

// TestParseAgentImages_ValidatesHarnessSetKeys: a malformed harness-set key in
// WARDYN_AGENT_IMAGES stops the daemon at boot rather than at a launch.
func TestParseAgentImages_ValidatesHarnessSetKeys(t *testing.T) {
	if _, err := parseAgentImages(`{"claude-code":"wardyn/agent-claude-code:local","claude-code+codex-cli":"wardyn/agent-harnesses:local"}`); err != nil {
		t.Fatalf("a valid set key: %v", err)
	}
	for _, raw := range []string{`{"codex-cli+claude-code":"x"}`, `{"claude-code+":"x"}`, `{"claude-code+claude-code":"x"}`} {
		if _, err := parseAgentImages(raw); err == nil {
			t.Errorf("%s was accepted", raw)
		}
	}
}
