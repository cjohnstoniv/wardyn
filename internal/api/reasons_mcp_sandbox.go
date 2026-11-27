// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "strings"

// The reasons and sentences of an MCP server that starts from a command, which runs only in a sandbox of its
// own: the same closed set and convention as reasons.go, in a file of its own beside run_mcp_sandbox_gate.go.
// TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonMCPOwnSandboxOff         = "mcp_own_sandbox_off"         // WARDYN_MCP_OWN_SANDBOX or the organisation's Local MCP servers setting is off, or the site config could not be read
	reasonMCPOwnSandboxUnsupported = "mcp_own_sandbox_unsupported" // the run's machine or runner cannot start an MCP server in a sandbox of its own
	reasonMCPSandboxLimit          = "mcp_sandbox_limit"           // the run has more MCP servers that start from a command than mcpSandboxMax
)

// The sentences, each the approved template byte for byte. <server> and <runner> are filled by
// mcpSandboxSentence.
const (
	mcpOwnSandboxOffSentence         = "MCP servers that start from a command are turned off here. Remove <server>, or ask an administrator."
	mcpOwnSandboxUnsupportedSentence = "<runner> can't start an MCP server in its own sandbox. Choose a runner that can, or remove <server>."
	mcpOwnSandboxMacSentence         = "A macOS VM run can't start an MCP server in its own sandbox. Choose another machine, or remove <server>."
	mcpOwnSandboxLinuxVMSentence     = "Local MCP servers can't run beside a Linux VM yet. Choose a container machine, use a remote (HTTPS) MCP server, or remove <server>."
	mcpSandboxLimitSentence          = "A run can have at most 4 MCP servers in their own sandboxes. Remove one, then launch again."
	mcpNarrowerWidensSentence        = "Narrower than the run can only remove access. Edit <server> to remove the extra hosts."
	mcpReviveOffSentence             = "<server> was not restarted: MCP servers that start from a command are turned off here. End the run, or ask an administrator."
	mcpReviveLegacySentence          = "<server> was not restarted: it ran inside the agent's sandbox, which this version no longer allows. End the run, then start a new one."
	// mcpSaveOffSentence is the component save's refusal while the switch is off, in the validator's form.
	mcpSaveOffSentence = "mcp.transport: MCP servers that start from a command are turned off here."
	// mcpRunnerUnnamed is the unsupported-runner sentence's runner when the run's runner has no name to show.
	mcpRunnerUnnamed = "This runner"
)

// mcpSandboxSentence fills a sentence's <server> and <runner>.
func mcpSandboxSentence(template, server, runnerName string) string {
	return strings.NewReplacer("<server>", server, "<runner>", runnerName).Replace(template)
}
