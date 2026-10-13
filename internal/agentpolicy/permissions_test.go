// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package agentpolicy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// attendedRules is the rule set every generated golden carries: one name per
// list, an MCP tool, a duplicate, and the "*" no permission rule can express.
var attendedRules = []types.ToolRule{
	{Tool: "WebFetch", Effect: types.ToolHold},
	{Tool: "Bash", Effect: types.ToolDeny},
	{Tool: "mcp__github__create_issue", Effect: types.ToolDeny},
	{Tool: "Read", Effect: types.ToolAllow},
	{Tool: "mcp__github__get_issue", Effect: types.ToolAllow},
	{Tool: "Read", Effect: types.ToolAllow},
	{Tool: "*", Effect: types.ToolHold},
}

// TestAgentPolicy_GeneratedPermissionsGolden: an interactive run's tool rules
// land in the document its level brings (the attended base where the level
// brings none) as Claude Code's permission lists, locked to managed rules
// only, byte for byte the files the pinned CLI was exercised with
// (testdata/attended-rules, outside the frozen documents' drift-probe glob).
// Every key the frozen document carried is still there with its value.
func TestAgentPolicy_GeneratedPermissionsGolden(t *testing.T) {
	for _, c := range []struct {
		golden string
		level  types.AutonomyLevel
		locked bool
	}{
		{"L0", types.AutonomyL0, false},
		{"L1", types.AutonomyL1, false},
		{"L2", types.AutonomyL2, false},
		{"L2-locked", types.AutonomyL2, true},
		{"no-level", "", false},
		{"no-level", types.AutonomyL3, false},
	} {
		t.Run(c.golden+"/"+string(c.level), func(t *testing.T) {
			_, base, _ := ForAgent("claude-code", c.level, false, c.locked)
			got, err := WithToolRules(base, attendedRules)
			if err != nil {
				t.Fatalf("WithToolRules: %v", err)
			}
			want, err := os.ReadFile(filepath.Join("testdata", "attended-rules", c.golden+".json"))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != string(want) {
				t.Errorf("generated managed settings differ from the verified golden.\n got: %s\nwant: %s", got, want)
			}
			if base == nil {
				base = []byte(claudeAttended)
			}
			var before, after map[string]any
			if err := json.Unmarshal(base, &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(got, &after); err != nil {
				t.Fatal(err)
			}
			for k, v := range before["permissions"].(map[string]any) {
				if after["permissions"].(map[string]any)[k] != v {
					t.Errorf("permissions.%s = %v, want the frozen document's %v", k, after["permissions"].(map[string]any)[k], v)
				}
			}
			for k, v := range before {
				if k != "permissions" && after[k] != v {
					t.Errorf("%s = %v, want the frozen document's %v", k, after[k], v)
				}
			}
			if after["allowManagedPermissionRulesOnly"] != true {
				t.Error("allowManagedPermissionRulesOnly unset: a repository or user rule could answer a call before the run's rules")
			}
		})
	}
}

// TestAgentPolicy_WithToolRulesFailsClosed: whatever stops the rules being
// written exactly is an error and no document, never a document without them.
func TestAgentPolicy_WithToolRulesFailsClosed(t *testing.T) {
	_, l1, _ := ForAgent("claude-code", types.AutonomyL1, false, false)
	for _, c := range []struct {
		name  string
		doc   []byte
		rules []types.ToolRule
	}{
		{"a pattern", l1, []types.ToolRule{{Tool: "Bash(rm:*)", Effect: types.ToolDeny}}},
		{"a whole MCP server", l1, []types.ToolRule{{Tool: "mcp__github", Effect: types.ToolAllow}}},
		{"an MCP server with no tool", l1, []types.ToolRule{{Tool: "mcp__github__", Effect: types.ToolAllow}}},
		{"a wildcard inside a name", l1, []types.ToolRule{{Tool: "mcp__github__*", Effect: types.ToolAllow}}},
		{"an unknown effect", l1, []types.ToolRule{{Tool: "Bash", Effect: "maybe"}}},
		{"an unreadable document", []byte(`{"permissions":`), []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}},
		{"a document with no permissions", []byte(`{}`), []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}},
		{"a list the document already sets", []byte(`{"permissions":{"deny":["Read"]}}`), []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, err := WithToolRules(c.doc, c.rules)
			if err == nil || got != nil {
				t.Errorf("WithToolRules = %q, %v; want no document and an error", got, err)
			}
		})
	}
}

// TestAgentPolicy_NoNamedRuleLeavesTheDocument: a run with no rule, or only
// "*", keeps the frozen document byte for byte, and no document where it had
// none.
func TestAgentPolicy_NoNamedRuleLeavesTheDocument(t *testing.T) {
	_, l0, _ := ForAgent("claude-code", types.AutonomyL0, false, false)
	for _, rules := range [][]types.ToolRule{nil, {{Tool: "*", Effect: types.ToolDeny}}} {
		if got, err := WithToolRules(l0, rules); err != nil || string(got) != string(l0) {
			t.Errorf("rules %v: %q, %v; want the L0 document unchanged", rules, got, err)
		}
		if got, err := WithToolRules(nil, rules); err != nil || got != nil {
			t.Errorf("rules %v on no document: %q, %v; want none", rules, got, err)
		}
	}
}

// TestAgentPolicy_DisablesBypassNamesTheSupervisedRungs: the documents that
// lock bypass off are L0's and L1's; L2's and the attended base leave a
// launch flag's bypass working, rules or not.
func TestAgentPolicy_DisablesBypassNamesTheSupervisedRungs(t *testing.T) {
	for _, c := range []struct {
		level  types.AutonomyLevel
		locked bool
		want   bool
	}{
		{types.AutonomyL0, false, true}, {types.AutonomyL1, false, true},
		{types.AutonomyL2, false, false}, {types.AutonomyL2, true, false}, {types.AutonomyL3, false, false}, {"", false, false},
	} {
		_, doc, _ := ForAgent("claude-code", c.level, false, c.locked)
		withRules, err := WithToolRules(doc, attendedRules)
		if err != nil {
			t.Fatal(err)
		}
		if DisablesBypass(doc) != c.want || DisablesBypass(withRules) != c.want {
			t.Errorf("%s locked=%v: DisablesBypass = %v / %v with rules, want %v", c.level, c.locked, DisablesBypass(doc), DisablesBypass(withRules), c.want)
		}
		if strings.Contains(string(withRules), "*") {
			t.Errorf("%s: the generated document carries a wildcard: %s", c.level, withRules)
		}
	}
}

// TestAgentPolicy_AcceptsEditsNamesTheUnattendedRung: L2's documents, locked or
// not and rules or not, start the session accepting file edits; no other does.
func TestAgentPolicy_AcceptsEditsNamesTheUnattendedRung(t *testing.T) {
	for _, c := range []struct {
		level  types.AutonomyLevel
		locked bool
		want   bool
	}{
		{types.AutonomyL0, false, false}, {types.AutonomyL1, false, false},
		{types.AutonomyL2, false, true}, {types.AutonomyL2, true, true}, {types.AutonomyL3, false, false}, {"", false, false},
	} {
		_, doc, _ := ForAgent("claude-code", c.level, false, c.locked)
		withRules, err := WithToolRules(doc, attendedRules)
		if err != nil {
			t.Fatal(err)
		}
		if AcceptsEdits(doc) != c.want || AcceptsEdits(withRules) != c.want {
			t.Errorf("%s locked=%v: AcceptsEdits = %v / %v with rules, want %v", c.level, c.locked, AcceptsEdits(doc), AcceptsEdits(withRules), c.want)
		}
	}
}
