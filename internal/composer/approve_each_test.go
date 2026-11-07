// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const approveEachTool = "mcp__jira__delete_issue"

func effectOf(rules []types.ToolRule, tool string) types.ToolEffect {
	for _, r := range rules {
		if r.Tool == tool {
			return r.Effect
		}
	}
	return ""
}

// TestClampRaisesHoldToCeilingApproveEach: a member who holds a tool the
// operator marks approve_each is raised to approve_each, because the person's
// terminal prompt is not the per-call approval the operator asked for.
func TestClampRaisesHoldToCeilingApproveEach(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.ToolRules = []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolApproveEach}}
	got, warns := Clamp(types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: approveEachTool, Effect: types.ToolHold},
	}}, ceiling, types.GovernanceLimits{})
	if e := effectOf(got.ToolRules, approveEachTool); e != types.ToolApproveEach {
		t.Fatalf("clamped effect = %q, want approve_each", e)
	}
	if !hasWarn(warns, `raised from hold to the operator's approve_each`) {
		t.Errorf("expected the raise to be said, got %v", warns)
	}
	// Allow is raised too, and a silent proposal inherits the ceiling's rule.
	got, _ = Clamp(types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolAllow}}}, ceiling, types.GovernanceLimits{})
	if e := effectOf(got.ToolRules, approveEachTool); e != types.ToolApproveEach {
		t.Errorf("allow clamped to %q, want approve_each", e)
	}
	got, _ = Clamp(types.RunPolicySpec{}, ceiling, types.GovernanceLimits{})
	if e := effectOf(got.ToolRules, approveEachTool); e != types.ToolApproveEach {
		t.Errorf("a silent proposal got %q, want the ceiling's approve_each", e)
	}
}

// TestClampKeepsMemberApproveEachOverCeilingHold: a member may narrow hold to
// approve_each, and deny stays stricter than both.
func TestClampKeepsMemberApproveEachOverCeilingHold(t *testing.T) {
	ceiling := operatorCeiling(t)
	ceiling.ToolRules = []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolHold}}
	got, warns := Clamp(types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: approveEachTool, Effect: types.ToolApproveEach},
	}}, ceiling, types.GovernanceLimits{})
	if e := effectOf(got.ToolRules, approveEachTool); e != types.ToolApproveEach {
		t.Fatalf("member approve_each over a hold ceiling became %q", e)
	}
	if hasWarn(warns, "tool_rules") {
		t.Errorf("a narrowing rule must not warn: %v", warns)
	}
	ceiling.ToolRules = []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolDeny}}
	got, _ = Clamp(types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: approveEachTool, Effect: types.ToolApproveEach}}}, ceiling, types.GovernanceLimits{})
	if e := effectOf(got.ToolRules, approveEachTool); e != types.ToolDeny {
		t.Errorf("approve_each under a deny ceiling became %q, want deny", e)
	}
}

func TestMeetToolRulesApproveEach(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{ToolRules: []types.ToolRule{
		{Tool: "*", Effect: types.ToolHold}, {Tool: approveEachTool, Effect: types.ToolHold}, {Tool: "mcp__jira__read", Effect: types.ToolApproveEach},
	}}}
	// A wall narrows hold to approve_each, and cannot soften an approve_each to hold.
	got := mustApply(t, base, overlayOf(types.CeilingOverlay{ToolRules: &[]types.ToolRule{
		{Tool: approveEachTool, Effect: types.ToolApproveEach}, {Tool: "mcp__jira__read", Effect: types.ToolHold},
	}}))
	if e := effectFor(toolEffects(got.Ceiling.ToolRules), approveEachTool); e != types.ToolApproveEach {
		t.Errorf("hold met with approve_each = %q, want approve_each", e)
	}
	if e := effectFor(toolEffects(got.Ceiling.ToolRules), "mcp__jira__read"); e != types.ToolApproveEach {
		t.Errorf("approve_each met with hold = %q, want approve_each", e)
	}
	// Deny beats approve_each.
	got = mustApply(t, base, overlayOf(types.CeilingOverlay{ToolRules: &[]types.ToolRule{{Tool: "mcp__jira__read", Effect: types.ToolDeny}}}))
	if e := effectFor(toolEffects(got.Ceiling.ToolRules), "mcp__jira__read"); e != types.ToolDeny {
		t.Errorf("approve_each met with deny = %q, want deny", e)
	}
	// A softening overlay is a widening and is said so.
	_, warns, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{ToolRules: &[]types.ToolRule{{Tool: "mcp__jira__read", Effect: types.ToolAllow}}}))
	if err != nil || !hasWarn(warns, "tool_rules") {
		t.Errorf("allow over approve_each: err=%v warns=%v, want a tool_rules widening warning", err, warns)
	}
	// approve_each only names one exact MCP tool.
	for _, tool := range []string{"*", "Bash", "mcp__jira"} {
		_, _, err := ApplyOverlay(base, overlayOf(types.CeilingOverlay{ToolRules: &[]types.ToolRule{{Tool: tool, Effect: types.ToolApproveEach}}}))
		wantOverlayErr(t, err, ReasonOverlayInvalid, "tool_rules")
	}
}

func TestLeqToolRulesApproveEach(t *testing.T) {
	spec := func(e types.ToolEffect) types.RunPolicySpec {
		return types.RunPolicySpec{ToolRules: []types.ToolRule{{Tool: "*", Effect: types.ToolHold}, {Tool: approveEachTool, Effect: e}}}
	}
	var none types.GovernanceLimits
	for _, c := range []struct {
		a, b types.ToolEffect
		want bool
	}{
		{types.ToolApproveEach, types.ToolHold, true},
		{types.ToolApproveEach, types.ToolAllow, true},
		{types.ToolApproveEach, types.ToolApproveEach, true},
		{types.ToolDeny, types.ToolApproveEach, true},
		{types.ToolHold, types.ToolApproveEach, false},
		{types.ToolAllow, types.ToolApproveEach, false},
		{types.ToolApproveEach, types.ToolDeny, false},
	} {
		if got := Leq(spec(c.a), spec(c.b), none, none); got != c.want {
			t.Errorf("Leq(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

// TestToolStrictnessIsTheSharedOrder: the composer has no order of its own.
func TestToolStrictnessIsTheSharedOrder(t *testing.T) {
	for _, e := range []types.ToolEffect{"", types.ToolAllow, types.ToolHold, types.ToolApproveEach, types.ToolDeny, "maybe"} {
		if got, want := toolStrictness(e), types.ToolEffectStrictness(e); got != want {
			t.Errorf("toolStrictness(%q) = %d, shared order says %d", e, got, want)
		}
	}
}
