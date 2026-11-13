// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

// A host rule or an MCP server URL that writes an address in a non-standard spelling is refused: the
// proxy refuses every request written that way, and a request for the standard spelling is another
// host, so the rule or the brokered-only refusal would bind nothing.
func TestHostRuleAndMCPURLRefuseANonStandardIPSpelling(t *testing.T) {
	const want = "four decimal numbers from 0 to 255 with no leading zeros"
	for _, h := range []string{"134744072", "0x8080808", "010.0.0.1", "8.8.8.008", "0x7f.1", "1.2.3", "0x"} {
		err := ValidateHostRuleSets([]HostRuleSet{{Rules: []HostRule{{Host: h, Paths: []string{"/ok"}, Effect: HostRuleAllow}}}})
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("a host rule for %q = %v, want the spelling refusal", h, err)
		}
		if _, err := MCPURLDestination("https://" + h + "/mcp"); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("an MCP URL at %q = %v, want the spelling refusal", h, err)
		}
	}
	for _, h := range []string{"8.8.8.8", "example.com", "0x8.example.com", "123.example.com"} {
		if err := ValidateHostRuleSets([]HostRuleSet{{Rules: []HostRule{{Host: h, Paths: []string{"/ok"}, Effect: HostRuleAllow}}}}); err != nil {
			t.Errorf("a host rule for %q = %v, want nil", h, err)
		}
		if _, err := MCPURLDestination("https://" + h + "/mcp"); err != nil {
			t.Errorf("an MCP URL at %q = %v, want nil", h, err)
		}
	}
}
