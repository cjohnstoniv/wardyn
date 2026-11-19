// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// approveEachLocalMCPRefusal refuses a run with an approve_each rule on a tool of an MCP server
// that runs in the sandbox, for every harness: only the broker can hold a call for a person, and
// it never sees a local server's calls, so the rule would be dropped silently. The rules read are
// the policy's and every one the person sent. Codex CLI and a command's harness are not refused
// for a remote server's tool: the broker pauses the call, not the harness.
func approveEachLocalMCPRefusal(req createRunRequest, spec types.RunPolicySpec, comps runComponents) *runRefusal {
	rules := slices.Clone(spec.ToolRules)
	for _, t := range req.Tools {
		rules = append(rules, t.ToolRules...)
	}
	if o := req.Overrides; o != nil && o.Agent != nil {
		rules = append(rules, o.Agent.ToolRules...)
	}
	if !slices.ContainsFunc(rules, isApproveEach) {
		return nil
	}
	keys := mcpServerKeys(comps.attached)
	for i, a := range comps.attached {
		if !a.snapshot.Definition.LocalMCP() {
			continue
		}
		prefixes := proxy.MCPToolPrefixes(keys[i])
		if slices.ContainsFunc(rules, func(r types.ToolRule) bool {
			return isApproveEach(r) && slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(r.Tool, p) })
		}) {
			return runError(http.StatusUnprocessableEntity, reasonRequestFieldUnavailable, approveEachLocalMCPSentence(keys[i]))
		}
	}
	return nil
}

// approveEachLocalMCPSentence is approveEachLocalMCPRefusal's text for the server's key.
func approveEachLocalMCPSentence(server string) string {
	return fmt.Sprintf("Wardyn can ask for each call only of an MCP server it reaches through the proxy; %s runs in the sandbox.", server)
}

func isApproveEach(r types.ToolRule) bool { return r.Effect == types.ToolApproveEach }

// attendedBrokerAsks reports whether tool is one of a remote MCP server turned on for Claude Code,
// the calls the broker holds for a person.
func attendedBrokerAsks(comps componentDispatch, tool string) bool {
	return slices.ContainsFunc(comps.MCP, func(m componentMCP) bool {
		return m.Launch == nil && m.URL != "" && slices.Contains(m.Harnesses, claudeCodeHarness) &&
			slices.ContainsFunc(proxy.MCPToolPrefixes(m.Key), func(p string) bool { return strings.HasPrefix(tool, p) })
	})
}

// mapAttendedApproveEach is rules as Claude Code's managed settings can carry them. Its permission
// lists have no "ask for each call", so each approve_each rule becomes allow where the broker asks
// (a remote server turned on for Claude Code) and deny otherwise, since nothing else could ask with
// the arguments. The rules are copied; the caller's slice is never changed.
func mapAttendedApproveEach(rules []types.ToolRule, comps componentDispatch) []types.ToolRule {
	if !slices.ContainsFunc(rules, isApproveEach) {
		return rules
	}
	out := slices.Clone(rules)
	for i, r := range out {
		if !isApproveEach(r) {
			continue
		}
		out[i].Effect = types.ToolDeny
		if attendedBrokerAsks(comps, r.Tool) {
			out[i].Effect = types.ToolAllow
		}
	}
	return out
}

// approveEachReasons adds approve_each to the reasons a harness's tool facts grey an effect with:
// never the default (it names one MCP tool), and a rule only where the harness has a remote MCP
// server for the broker to ask through. An effect the harness already cannot take keeps its reason.
func approveEachReasons(forDefault, forRule map[types.ToolEffect]string, remote bool) (map[types.ToolEffect]string, map[types.ToolEffect]string) {
	d, r := maps.Clone(forDefault), maps.Clone(forRule)
	if d == nil {
		d = map[types.ToolEffect]string{}
	}
	if r == nil {
		r = map[types.ToolEffect]string{}
	}
	if d[types.ToolApproveEach] == "" {
		d[types.ToolApproveEach] = "approve_each_default"
	}
	if r[types.ToolApproveEach] == "" && !remote {
		r[types.ToolApproveEach] = "approve_each_no_remote_mcp"
	}
	return d, r
}
