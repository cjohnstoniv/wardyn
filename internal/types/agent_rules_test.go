// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestAgentRulesEmpty(t *testing.T) {
	denied := []string{"evil.example.com"}
	for _, c := range []struct {
		name  string
		rules AgentRules
		empty bool
	}{
		{"zero value", AgentRules{}, true},
		{"inner walls of two empty strings", AgentRules{InnerWalls: &HarnessInnerWalls{}}, true},
		{"one denied domain", AgentRules{Spec: PersonalWallsSpec{DeniedDomains: &denied}}, false},
		{"a sandbox mode", AgentRules{InnerWalls: &HarnessInnerWalls{SandboxMode: InnerSandboxReadOnly}}, false},
	} {
		if got := c.rules.Empty(); got != c.empty {
			t.Errorf("%s: Empty() = %v, want %v", c.name, got, c.empty)
		}
	}
}
