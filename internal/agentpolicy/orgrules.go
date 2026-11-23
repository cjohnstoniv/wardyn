// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package agentpolicy

import (
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// noHarness is "Your own tools": a run with no agent CLI, which no agent's rules bind.
const noHarness = "none"

// RulesFor is the organisation's rules (all) that bind a run including harnesses: one per harness, in
// harness id order, so a run's layers are applied and recorded the same way on every read. A harness with
// no rules, "" and "none" take none.
func RulesFor(harnesses []string, all []types.AgentRules) []types.AgentRules {
	var out []types.AgentRules
	for _, r := range all {
		if r.Harness == "" || r.Harness == noHarness || !slices.Contains(harnesses, r.Harness) {
			continue
		}
		if slices.ContainsFunc(out, func(o types.AgentRules) bool { return o.Harness == r.Harness }) {
			continue
		}
		out = append(out, r)
	}
	slices.SortFunc(out, func(a, b types.AgentRules) int { return strings.Compare(a.Harness, b.Harness) })
	return out
}

// sandboxStrictness ranks the harness sandbox modes, the strictest highest.
var sandboxStrictness = map[string]int{types.InnerSandboxReadOnly: 3, types.InnerSandboxWorkspaceWrite: 2, types.InnerSandboxOff: 1}

// MeetInnerWalls is a policy's harness_inner_walls choice (policy, nil for none) met with an organisation
// rule's (rule). The sandbox mode is the stricter of the two. Approvals the rule sets must be the policy's
// when the policy sets them too: axis names the axis that disagrees, and the run is refused. A rule that
// sets nothing returns policy as it came.
func MeetInnerWalls(policy, rule *types.HarnessInnerWalls) (met *types.HarnessInnerWalls, axis string) {
	if rule == nil || *rule == (types.HarnessInnerWalls{}) {
		return policy, ""
	}
	var out types.HarnessInnerWalls
	if policy != nil {
		out = *policy
	}
	if rule.SandboxMode != "" && sandboxStrictness[rule.SandboxMode] > sandboxStrictness[out.SandboxMode] {
		out.SandboxMode = rule.SandboxMode
	}
	if rule.Approvals != "" {
		if out.Approvals != "" && out.Approvals != rule.Approvals {
			return nil, "approvals"
		}
		out.Approvals = rule.Approvals
	}
	return &out, ""
}

// LoosensInnerWalls reports whether a rule's inner walls name a value that can only loosen a run: the
// harness's sandbox off, or no approvals. An organisation rule never names either.
func LoosensInnerWalls(w *types.HarnessInnerWalls) bool {
	return w != nil && (w.SandboxMode == types.InnerSandboxOff || w.Approvals == types.InnerApprovalsNone)
}
