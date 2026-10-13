// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// accessSentences is the canon text behind every closed code step 5 shows:
// the reason codes a row or a tool option carries, the delivery labels of
// AccessCredential.delivery, and the lines for a host nothing lists. A
// {placeholder} is filled by the reader; {a|b} is a choice it makes. The
// console's twin is pinned to the same table (access-reasons.golden.json,
// TestAccessReasonSentencesMatchGolden).
//
// The unlisted keys are "unlisted.<mode>.<decider>" for the two review modes
// (AccessUnlisted.Mode and .Decider), "unlisted.always_deny" and
// "unlisted.allow_all"; "unlisted.hold_timeout" follows a wait_for_review line,
// and the list's second sentence is "unlisted.requested" until the server has
// answered the current draft, "unlisted.checked" after.
var accessSentences = map[string]string{
	"policy_stricter_default":    "Your policy {asks first for|blocks} tools with no rule. A run can only be stricter.",
	"policy_stricter_tool":       "Your policy {asks first for|blocks} {tool}. A run can only be stricter.",
	"harness_no_hold":            "Codex CLI can't pause a tool call for approval.",
	"harness_no_block_builtin":   "Wardyn can't block Codex CLI's own tools. Rules for its MCP tools still apply.",
	"attended_no_skip":           "In an interactive run Claude Code asks you before it acts, and this run's settings don't let it skip asking.",
	"attended_rules_unavailable": "Wardyn can't set tool rules for an interactive run yet.",
	"custom_harness_rules":       "Wardyn can't set tool rules for {harness}.",
	"mcp_harness_unsupported":    "Wardyn can't configure MCP servers for {harness}.",
	"cli_not_granted":            "Not turned on for you. Ask your admin.",
	"cli_no_github_repo":         "GitHub CLI works only with this run's GitHub repositories. Add one on the Files step.",
	"component_unavailable":      componentUnavailableRefusal,
	"resident_off":               "Your organisation has turned off environment-variable and file delivery. Use a request header.",

	"delivery." + deliveryProxy:       "Proxy only",
	"delivery." + deliveryGitBroker:   "Via Wardyn's git broker",
	"delivery." + deliveryGitHelper:   "Given to git when it asks",
	"delivery." + deliverySandboxEnv:  "In the sandbox as an environment variable",
	"delivery." + deliverySandboxFile: "In the sandbox as a file",
	"delivery." + deliveryShortLived:  "In the sandbox, short-lived",

	"unlisted.always_deny":                    "Anything else is refused and logged.",
	"unlisted.deny_with_review." + deciderYou: "Anything else is refused the first time the run asks for it and raises a request. Once you approve it, the run can try again.",
	"unlisted.deny_with_review." + deciderAdmin: "Anything else is refused the first time the run asks for it and raises a request. " +
		"Once an admin approves it, the run can try again.",
	"unlisted.deny_with_review." + deciderYouOrAdmin: "Anything else is refused the first time the run asks for it and raises a request. " +
		"Once you or an admin approves it, the run can try again.",
	"unlisted.wait_for_review." + deciderYou:        "Anything else is held for your review the first time the run asks for it.",
	"unlisted.wait_for_review." + deciderAdmin:      "Anything else is held for an admin's review the first time the run asks for it.",
	"unlisted.wait_for_review." + deciderYouOrAdmin: "Anything else is held for your or an admin's review the first time the run asks for it.",
	"unlisted.hold_timeout":                         "If nobody decides within {n} seconds, it's refused.",
	"unlisted.allow_all":                            "Any other public host is allowed, except ones your organisation blocks.",
	"unlisted.requested":                            "Your organisation's ceiling still narrows this list at review.",
	"unlisted.checked":                              "Checked against your organisation's limits just now.",
}
