// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package agentpolicy

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func harnesses(r []types.AgentRules) []string {
	var out []string
	for _, x := range r {
		out = append(out, x.Harness)
	}
	return out
}

func TestRulesFor(t *testing.T) {
	cc := types.AgentRules{Harness: "claude-code", Version: 1}
	cx := types.AgentRules{Harness: "codex-cli", Version: 1}
	for _, c := range []struct {
		name      string
		harnesses []string
		all       []types.AgentRules
		want      []string
	}{
		{"no rules", []string{"claude-code"}, nil, nil},
		{"one match", []string{"claude-code"}, []types.AgentRules{cc}, []string{"claude-code"}},
		{"a harness not in the run", []string{"codex-cli"}, []types.AgentRules{cc}, nil},
		{"none and empty are skipped", []string{"none", ""}, []types.AgentRules{{Harness: "none"}, {Harness: ""}}, nil},
		{"a duplicate row is kept once", []string{"claude-code"}, []types.AgentRules{cc, {Harness: "claude-code", Version: 2}}, []string{"claude-code"}},
		{"sorted by harness id", []string{"claude-code", "codex-cli"}, []types.AgentRules{cx, cc}, []string{"claude-code", "codex-cli"}},
	} {
		got := harnesses(RulesFor(c.harnesses, c.all))
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: harnesses = %v, want %v", c.name, got, c.want)
		}
	}
	if got := RulesFor([]string{"claude-code"}, []types.AgentRules{cc, {Harness: "claude-code", Version: 2}}); got[0].Version != 1 {
		t.Errorf("a duplicate keeps the first row, got version %d", got[0].Version)
	}
}

func TestMeetInnerWalls(t *testing.T) {
	w := func(sandbox, approvals string) *types.HarnessInnerWalls {
		return &types.HarnessInnerWalls{SandboxMode: sandbox, Approvals: approvals}
	}
	for _, c := range []struct {
		name     string
		policy   *types.HarnessInnerWalls
		rule     *types.HarnessInnerWalls
		want     *types.HarnessInnerWalls
		wantAxis string
	}{
		{"nil rule", w("read-only", ""), nil, w("read-only", ""), ""},
		{"nil rule and nil policy", nil, nil, nil, ""},
		{"empty rule", w("read-only", "harness"), w("", ""), w("read-only", "harness"), ""},
		{"rule over nil policy", nil, w("workspace-write", "wardyn"), w("workspace-write", "wardyn"), ""},
		{"read-only policy keeps read-only", w("read-only", ""), w("workspace-write", ""), w("read-only", ""), ""},
		{"off policy meets workspace-write", w("off", ""), w("workspace-write", ""), w("workspace-write", ""), ""},
		{"stricter rule wins", w("workspace-write", ""), w("read-only", ""), w("read-only", ""), ""},
		{"agreeing approvals", w("", "wardyn"), w("", "wardyn"), w("", "wardyn"), ""},
		{"disagreeing approvals", w("", "harness"), w("", "wardyn"), nil, "approvals"},
		{"a policy axis kept where the rule sets none", w("", "harness"), w("read-only", ""), w("read-only", "harness"), ""},
	} {
		got, axis := MeetInnerWalls(c.policy, c.rule)
		if axis != c.wantAxis || !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got (%+v, %q), want (%+v, %q)", c.name, got, axis, c.want, c.wantAxis)
		}
	}
}

func TestLoosensInnerWalls(t *testing.T) {
	for _, c := range []struct {
		name string
		w    *types.HarnessInnerWalls
		want bool
	}{
		{"sandbox off", &types.HarnessInnerWalls{SandboxMode: types.InnerSandboxOff}, true},
		{"approvals none", &types.HarnessInnerWalls{Approvals: types.InnerApprovalsNone}, true},
		{"read-only", &types.HarnessInnerWalls{SandboxMode: types.InnerSandboxReadOnly}, false},
		{"approvals wardyn", &types.HarnessInnerWalls{Approvals: types.InnerApprovalsWardyn}, false},
		{"nil", nil, false},
	} {
		if got := LoosensInnerWalls(c.w); got != c.want {
			t.Errorf("%s: LoosensInnerWalls = %v, want %v", c.name, got, c.want)
		}
	}
}
