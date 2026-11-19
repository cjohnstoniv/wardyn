// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

// TestToolEffectStrictnessOrder pins the one order every layer shares:
// "" (no opinion) < allow < hold < approve_each < deny, and anything unknown
// ranks with deny so a new effect a table forgot fails closed.
func TestToolEffectStrictnessOrder(t *testing.T) {
	ordered := []ToolEffect{"", ToolAllow, ToolHold, ToolApproveEach, ToolDeny}
	for i := 1; i < len(ordered); i++ {
		lo, hi := ToolEffectStrictness(ordered[i-1]), ToolEffectStrictness(ordered[i])
		if lo >= hi {
			t.Errorf("strictness(%q)=%d must be below strictness(%q)=%d", ordered[i-1], lo, ordered[i], hi)
		}
	}
	if got := ToolEffectStrictness("maybe"); got != ToolEffectStrictness(ToolDeny) {
		t.Errorf("an unknown effect ranks %d, want deny's %d", got, ToolEffectStrictness(ToolDeny))
	}
	if !ValidToolEffect(ToolApproveEach) {
		t.Error("approve_each must be a valid effect")
	}
	if ValidToolEffect("") || ValidToolEffect("maybe") {
		t.Error("the empty and unknown effects must stay invalid")
	}
}

// TestToolEffectStrictnessEmptyBelowAllow: callers start from "" (no opinion)
// and take the stricter, so "" must lose to every real effect, allow included.
func TestToolEffectStrictnessEmptyBelowAllow(t *testing.T) {
	if ToolEffectStrictness("") >= ToolEffectStrictness(ToolAllow) {
		t.Fatalf(`"" ranks %d, allow ranks %d: no opinion must rank strictly below allow`,
			ToolEffectStrictness(""), ToolEffectStrictness(ToolAllow))
	}
}

func TestValidToolRuleApproveEach(t *testing.T) {
	cases := []struct {
		name string
		rule ToolRule
		ok   bool
	}{
		{"an exact MCP tool", ToolRule{Tool: "mcp__x__y", Effect: ToolApproveEach}, true},
		{"a dotted raw MCP tool", ToolRule{Tool: "mcp__jira__delete.issue", Effect: ToolApproveEach}, true},
		{"a built-in tool", ToolRule{Tool: "Bash", Effect: ToolApproveEach}, false},
		{"the default", ToolRule{Tool: "*", Effect: ToolApproveEach}, false},
		{"a server with no tool", ToolRule{Tool: "mcp__x", Effect: ToolApproveEach}, false},
		{"a server prefix only", ToolRule{Tool: "mcp__x__", Effect: ToolApproveEach}, false},
		{"a pattern", ToolRule{Tool: "mcp__x__*", Effect: ToolApproveEach}, false},
		{"an empty tool", ToolRule{Tool: "", Effect: ToolApproveEach}, false},
		{"hold on a built-in tool", ToolRule{Tool: "Bash", Effect: ToolHold}, true},
		{"allow on the default", ToolRule{Tool: "*", Effect: ToolAllow}, true},
		{"an unknown effect", ToolRule{Tool: "Bash", Effect: "maybe"}, false},
		{"an empty effect", ToolRule{Tool: "Bash"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := ValidToolRule(c.rule)
			if (err == nil) != c.ok {
				t.Fatalf("ValidToolRule(%+v) = %v, want ok=%v", c.rule, err, c.ok)
			}
		})
	}
}

func TestValidateHarnessToolRulesRefusesApproveEachDefault(t *testing.T) {
	err := ValidateHarnessToolRules([]HarnessToolRules{{Harness: "codex-cli", Default: ToolApproveEach}})
	if err == nil || !strings.Contains(err.Error(), "approve_each") {
		t.Fatalf("a harness default of approve_each must be refused naming the effect, got %v", err)
	}
	if err := ValidateHarnessToolRules([]HarnessToolRules{{Harness: "codex-cli", Rules: []ToolRule{{Tool: "Bash", Effect: ToolApproveEach}}}}); err == nil {
		t.Error("approve_each on a built-in tool must be refused in a harness set")
	}
	if err := ValidateHarnessToolRules([]HarnessToolRules{{Harness: "codex-cli", Default: ToolHold,
		Rules: []ToolRule{{Tool: "mcp__x__y", Effect: ToolApproveEach}}}}); err != nil {
		t.Errorf("approve_each on an exact MCP tool is valid in a harness set: %v", err)
	}
}
