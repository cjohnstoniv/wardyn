// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// applyHarnessToolRules lays the included harness tools' own rules onto the
// resolved spec (whose tool_rules already are the policy's met with the
// ceiling's by composer.Clamp). With one harness the run keeps the run-wide
// regime, exactly as before per-tool rules existed: the tool's rules and default
// effect are compiled into spec.ToolRules and nothing per-harness is carried.
// With two or more, each harness gets a set of its own and the proxy tells them
// apart by credential. Either way compileHarnessRules keeps each tool at the
// stricter of the policy's effect and the person's, so nothing a person writes
// loosens the policy's or the ceiling's. laneDefault is toolLaneDefault's.
func applyHarnessToolRules(spec types.RunPolicySpec, tools []client.IncludedTool, laneDefault types.ToolEffect) (types.RunPolicySpec, []types.HarnessToolRules) {
	var harnesses []client.IncludedTool
	for _, t := range tools {
		if t.Kind == client.IncludedToolHarness {
			harnesses = append(harnesses, t)
		}
	}
	switch {
	case len(harnesses) == 0:
		return spec, nil
	case len(harnesses) == 1:
		t := harnesses[0]
		if !hasPersonalToolRules(t) {
			return spec, nil
		}
		spec = spec.Clone()
		spec.ToolRules = compileHarnessRules(spec.ToolRules, laneDefault, t)
		return spec, nil
	}
	if len(spec.ToolRules) == 0 && !slices.ContainsFunc(harnesses, hasPersonalToolRules) {
		return spec, nil
	}
	var out []types.HarnessToolRules
	for _, t := range harnesses {
		set := types.HarnessToolRules{Harness: t.ID}
		for _, r := range compileHarnessRules(spec.ToolRules, laneDefault, t) {
			if r.Tool == "*" {
				set.Default = r.Effect
				continue
			}
			set.Rules = append(set.Rules, r)
		}
		out = append(out, set)
	}
	return spec, out
}

// hasPersonalToolRules reports whether the person set a rule or a default on t.
func hasPersonalToolRules(t client.IncludedTool) bool {
	return len(t.ToolRules) > 0 || t.DefaultEffect != ""
}

// compileHarnessRules is one harness's flat rule list for the proxy, the two
// layers met per tool: each tool either layer names gets the stricter of the
// policy's effect for it (policyToolEffect) and the person's (their rule for it,
// else their default), and "*" the stricter of the two defaults. "*" is written
// only when the policy or the person sets a default: otherwise the lane's own
// handling of an unnamed tool is the default, as before.
//
// A person's rule may be looser than their own default (an exception, such as
// "block everything but Read") and still never looser than the policy, which a
// flat narrowing of one list cannot express.
func compileHarnessRules(policy []types.ToolRule, laneDefault types.ToolEffect, personal client.IncludedTool) []types.ToolRule {
	var tools []string
	for _, r := range slices.Concat(policy, personal.ToolRules) {
		if r.Tool != "*" && !slices.Contains(tools, r.Tool) {
			tools = append(tools, r.Tool)
		}
	}
	out := make([]types.ToolRule, 0, len(tools)+1)
	for _, tool := range tools {
		mine := personal.DefaultEffect
		if i := slices.IndexFunc(personal.ToolRules, func(r types.ToolRule) bool { return r.Tool == tool }); i >= 0 {
			mine = personal.ToolRules[i].Effect
		}
		out = append(out, types.ToolRule{Tool: tool, Effect: stricterToolEffect(policyToolEffect(policy, laneDefault, tool), mine)})
	}
	if personal.DefaultEffect != "" || slices.ContainsFunc(policy, func(r types.ToolRule) bool { return r.Tool == "*" }) {
		out = append(out, types.ToolRule{Tool: "*", Effect: stricterToolEffect(policyToolEffect(policy, laneDefault, "*"), personal.DefaultEffect)})
	}
	return out
}

// policyToolEffect is the policy layer's effect for a tool: its exact rule, else
// the policy's "*", else what the lane does with a tool no rule names.
func policyToolEffect(policy []types.ToolRule, laneDefault types.ToolEffect, tool string) types.ToolEffect {
	if eff := effectiveToolEffect(policy, tool); eff != "" {
		return eff
	}
	return laneDefault
}

// stricterToolEffect is the stricter of two effects ("" is no opinion).
func stricterToolEffect(a, b types.ToolEffect) types.ToolEffect {
	if effectRank(b) > effectRank(a) {
		return b
	}
	return a
}

// toolLaneDefault is what the run's lane does with a tool no rule names: the
// gated lane holds it for a human, and in an interactive run Claude Code asks
// the person at the terminal, which counts as a hold; an ungated run runs it.
func toolLaneDefault(req createRunRequest) types.ToolEffect {
	if requestIsInteractive(req) || holdLane(false, req.ToolApprovals, req.TaskMode) {
		return types.ToolHold
	}
	return types.ToolAllow
}

// applyToolPosture puts a background run of a harness with Wardyn's gate on the
// gated lane when the person sets any tool rule or default, or the policy (any
// layer, after the ceiling) holds or blocks a tool: only that lane consults the
// rules (holdLane), so off it a rule would be shown and never read.
// "*: allow" laid under the person's layer (provenance posture) keeps every tool
// nobody names running as it did ungated, so the run is never looser than
// before. A run already on the gated lane (an explicit hold, the profile's or the
// autonomy level's) keeps its own default, a hold.
func (s *Server) applyToolPosture(req *createRunRequest, spec *types.RunPolicySpec) {
	if requestIsInteractive(*req) || req.TaskMode == "exec" || !agentHasHoldLane(req.Agent) || req.ToolApprovals == "hold" {
		return
	}
	if !slices.ContainsFunc(req.Tools, hasPersonalToolRules) && !slices.ContainsFunc(spec.ToolRules, func(r types.ToolRule) bool {
		return r.Effect == types.ToolHold || r.Effect == types.ToolDeny
	}) {
		return
	}
	req.ToolApprovals = "hold"
	if !slices.ContainsFunc(spec.ToolRules, func(r types.ToolRule) bool { return r.Tool == "*" }) {
		*spec = spec.Clone()
		spec.ToolRules = append(spec.ToolRules, types.ToolRule{Tool: "*", Effect: types.ToolAllow})
	}
}

// toolRulesWithinPolicyRefusal holds every allow a person sends to the policy: a
// rule's allow (an exception to their own default, or an override) and an
// explicit allow default stand only where the policy, with the lane's default
// under it, already allows that tool. Read after the lane is settled, since the
// lane's default is the policy's for a tool the policy does not name.
func (s *Server) toolRulesWithinPolicyRefusal(req createRunRequest, spec types.RunPolicySpec) *runRefusal {
	var allows []string
	for _, t := range req.Tools {
		if t.DefaultEffect == types.ToolAllow {
			allows = append(allows, "*")
		}
		for _, r := range t.ToolRules {
			if r.Effect == types.ToolAllow {
				allows = append(allows, r.Tool)
			}
		}
	}
	if o := req.Overrides; o != nil && o.Agent != nil {
		for _, r := range o.Agent.ToolRules {
			if r.Effect == types.ToolAllow {
				allows = append(allows, r.Tool)
			}
		}
	}
	lane := toolLaneDefault(req)
	for _, tool := range allows {
		if eff := policyToolEffect(spec.ToolRules, lane, tool); eff != types.ToolAllow {
			return runError(http.StatusUnprocessableEntity, reasonOverrideRefused, policyStricterMsg(tool, eff))
		}
	}
	return nil
}

// inheritedMCPHoldRefusal refuses a run whose resolved policy explicitly holds a
// tool of an MCP server bound to a harness nothing can pause, as
// harnessToolRulesRefusal refuses the person's own hold: a background Codex CLI
// run has no approval contract and no gated lane, every harness a command
// starts is off the gated lane, and the MCP broker passes a held call, so
// nothing would ask.
func inheritedMCPHoldRefusal(req createRunRequest, spec types.RunPolicySpec, comps runComponents) *runRefusal {
	var harnesses []string
	sentence := accessSentences["harness_no_hold"]
	switch {
	case req.TaskMode == "exec":
		sentence, harnesses = accessSentences["command_no_hold"], mcpIncludedHarnesses(req)
	case req.Agent == codexHarness && !requestIsInteractive(req):
		harnesses = []string{codexHarness}
	}
	for _, h := range harnesses {
		if slices.ContainsFunc(mcpServersFor(comps, h), func(key string) bool { return policyHoldsMCPServer(spec.ToolRules, key) }) {
			return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, sentence)
		}
	}
	return nil
}

// localMCPRulesRefusal refuses a run whose rules hold or block a tool of a
// local MCP server turned on for a harness that cannot apply them: Codex CLI
// in any mode (it cannot pause a call, and Wardyn cannot block a tool whose
// calls never leave the sandbox), or any harness a command starts (off the
// gated lane, with no managed settings). The broker never sees a local
// server's calls, so the rule would otherwise be dropped silently. A rule
// names the server's tools by its prefix, or every tool by "*"; the person's
// own rules and default for that harness count with the policy's. An allow is
// what happens anyway, so it stands.
func localMCPRulesRefusal(req createRunRequest, spec types.RunPolicySpec, comps runComponents) *runRefusal {
	keys := mcpServerKeys(comps.attached)
	for i, a := range comps.attached {
		if !a.snapshot.Definition.LocalMCP() {
			continue
		}
		for _, h := range a.snapshot.MCPHarnesses {
			if h != codexHarness && req.TaskMode != "exec" {
				continue
			}
			rules := slices.Clone(spec.ToolRules)
			for _, t := range req.Tools {
				if t.ID == h && t.Kind == client.IncludedToolHarness {
					rules = append(rules, t.ToolRules...)
					if t.DefaultEffect != "" {
						rules = append(rules, types.ToolRule{Tool: "*", Effect: t.DefaultEffect})
					}
				}
			}
			if tool := mcpServerRestricted(rules, keys[i]); tool != "" {
				name := h
				if d, ok := harnessByID(h); ok {
					name = d.Display
				}
				return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, fmt.Sprintf(
					"In this run %s can't apply the rule for %s to the MCP server %q, which runs in the sandbox where Wardyn's proxy never sees its calls. Remove the rule, or turn the server off for %s.",
					name, tool, keys[i], name))
			}
		}
	}
	return nil
}

// mcpServerRestricted is the first rule's tool that holds or blocks a tool of
// the MCP server key ("*" for a default), "" when none does.
func mcpServerRestricted(rules []types.ToolRule, key string) string {
	prefixes := proxy.MCPToolPrefixes(key)
	for _, r := range rules {
		if (r.Effect == types.ToolHold || r.Effect == types.ToolDeny) &&
			(r.Tool == "*" || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(r.Tool, p) })) {
			return r.Tool
		}
	}
	return ""
}

// policyHoldsMCPServer reports whether rules explicitly hold a tool of the MCP
// server key: a hold naming one of its tools, or a hold default ("*"). The
// broker takes the exact rule for the tool called, else the default, and the
// server's tools are not known here, so an exact allow lifts the default from
// that one tool and never from the server. No rule at all is the broker's
// fallback, not an explicit hold.
func policyHoldsMCPServer(rules []types.ToolRule, key string) bool {
	prefixes := proxy.MCPToolPrefixes(key)
	return slices.ContainsFunc(rules, func(r types.ToolRule) bool {
		return r.Effect == types.ToolHold &&
			(r.Tool == "*" || slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(r.Tool, p) }))
	})
}

// harnessToolRulesRefusal refuses a rule or default the included harness has no
// way to enforce. Codex CLI cannot pause a call, and Wardyn sees only its MCP
// tools (its own tools have no approval contract); a harness without Wardyn's
// gate takes no rules at all.
func harnessToolRulesRefusal(t client.IncludedTool) *runRefusal {
	unavailable := func(msg string) *runRefusal {
		return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, msg)
	}
	switch {
	case !hasPersonalToolRules(t) || agentHasHoldLane(t.ID):
		return nil
	case t.ID != codexHarness:
		name := t.ID
		if d, ok := harnessByID(t.ID); ok {
			name = d.Display
		}
		return unavailable(strings.Replace(accessSentences["custom_harness_rules"], "{harness}", name, 1))
	case t.DefaultEffect == types.ToolHold || slices.ContainsFunc(t.ToolRules, func(r types.ToolRule) bool { return r.Effect == types.ToolHold }):
		return unavailable(accessSentences["harness_no_hold"])
	case t.DefaultEffect == types.ToolDeny:
		return unavailable(accessSentences["harness_no_block_builtin"])
	}
	for _, r := range t.ToolRules {
		if !strings.HasPrefix(r.Tool, "mcp__") {
			return unavailable(codexBuiltinRuleMsg(r.Tool))
		}
	}
	return nil
}

// codexHarness and claudeCodeHarness are Codex CLI's and Claude Code's harness ids.
const (
	codexHarness      = "codex-cli"
	claudeCodeHarness = "claude-code"
)

// exactToolNameRE is one tool's exact name, as the harness reports it (Bash,
// WebFetch, mcp__server__tool). Rules never match a pattern (types.ToolRule).
var exactToolNameRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// narrowToolRule adds nr to rules when it is stricter than the tool's effective
// effect there (its exact rule, else "*"): it replaces the exact rule, or is
// appended. replaced is the exact rule's old effect ("" if none); applied says
// whether anything changed. An allow never narrows, so it is never applied:
// whether one is within the policy is the fold's question
// (toolRulesWithinPolicyRefusal), and within it the allow is already there.
func narrowToolRule(rules []types.ToolRule, nr types.ToolRule) (out []types.ToolRule, replaced types.ToolEffect, applied bool) {
	if nr.Effect == types.ToolAllow || effectRank(nr.Effect) <= effectRank(effectiveToolEffect(rules, nr.Tool)) {
		return rules, "", false
	}
	out = slices.Clone(rules)
	if i := slices.IndexFunc(out, func(r types.ToolRule) bool { return r.Tool == nr.Tool }); i >= 0 {
		replaced, out[i].Effect = out[i].Effect, nr.Effect
		return out, replaced, true
	}
	return append(out, nr), "", true
}

// effectRank orders no opinion < allow < hold < deny.
func effectRank(e types.ToolEffect) int {
	return map[types.ToolEffect]int{"": 0, types.ToolAllow: 1, types.ToolHold: 2, types.ToolDeny: 3}[e]
}

// effectiveToolEffect is what a rule list says about a tool: its exact rule,
// else the "*" default, else nothing. The lookup order of proxy.Policy.ToolEffectFor
// and composer.ceilingToolEffect.
func effectiveToolEffect(rules []types.ToolRule, tool string) types.ToolEffect {
	var def types.ToolEffect
	for _, r := range rules {
		if r.Tool == tool {
			return r.Effect
		}
		if r.Tool == "*" {
			def = r.Effect
		}
	}
	return def
}

// mintHarnessCredentials makes one random credential per harness that has rules.
// The proxy config gets the digests; the plaintext of the harness the run starts
// (agent) is returned for its own process, and the others' are dropped here: a
// harness that is only included holds no credential until it is started.
func mintHarnessCredentials(sets []types.HarnessToolRules, agent string) (digests []types.HarnessCredential, agentCredential string, err error) {
	for _, s := range sets {
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return nil, "", err
		}
		cred := base64.RawURLEncoding.EncodeToString(raw)
		sum := sha256.Sum256([]byte(cred))
		digests = append(digests, types.HarnessCredential{Harness: s.Harness, SHA256: hex.EncodeToString(sum[:])})
		if s.Harness == agent {
			agentCredential = cred
		}
	}
	return digests, agentCredential, nil
}

// sidecarPolicy is the policy the proxy sidecar is given: the per-entry push rule
// sets and the per-harness tool rules replace the run-wide blocks (the sidecar
// refuses a config that carries both).
func sidecarPolicy(policy types.RunPolicySpec, p dispatchParams) types.RunPolicySpec {
	if len(p.PushRuleSets) > 0 {
		policy.PushRules = nil
	}
	if len(p.HarnessToolRules) > 0 {
		policy.ToolRules = nil
	}
	return policy
}
