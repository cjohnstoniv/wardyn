// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "strings"

// The reasons and sentences of an MCP server that starts from a command, which runs only in a sandbox of its
// own: the same closed set and convention as reasons.go, in a file of its own beside run_mcp_sandbox_gate.go.
// TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonMCPOwnSandboxOff         = "mcp_own_sandbox_off"         // WARDYN_MCP_OWN_SANDBOX or the organisation's Local MCP servers setting is off, or the site config could not be read
	reasonMCPOwnSandboxUnsupported = "mcp_own_sandbox_unsupported" // the run's machine or runner cannot start an MCP server in a sandbox of its own, or the site's egress goes through an upstream proxy, which an MCP sandbox's proxy does not use
	reasonMCPSandboxLimit          = "mcp_sandbox_limit"           // the run has more MCP servers that start from a command than mcpSandboxMax
	reasonMCPSandboxHostReachable  = "mcp_sandbox_host_reachable"  // the Docker Engine is older than 28.0.0, so an MCP server's sandbox would have an address on the Docker host (runner.ErrMCPSandboxHostReachable); no setting admits one
	reasonMCPSandboxApprovalCap    = "mcp_sandbox_approval_cap"    // an MCP server's proxy already holds maxMCPHoldsPerServer egress holds on its run (429)
	reasonMCPSandboxRunEnded       = "mcp_sandbox_run_ended"       // a Restart or Stop of an MCP server on a run that is not live: ended, lost, kept, containment pending or cut on this replica (409)
	reasonMCPSandboxStarting       = "mcp_sandbox_starting"        // a Restart of an MCP server that is still starting (409): nothing was killed
	reasonMCPSandboxUnavailable    = "mcp_sandbox_unavailable"     // a Restart whose read or write of the run's MCP servers failed (503)
	// An MCP proxy's token names no live server row of its run (403), or a grant its row does not hold. An
	// auth.fail reason, so its line carries no comment: TestAuthFailedReasonEnumIsDocumented reads it bare.
	reasonMCPSandboxUnknown = "mcp_sandbox_unknown"
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
	// mcpUpstreamProxySentence is the admission refusal on a site with an upstream proxy.
	// PROPOSED: this sentence is not in the approved strings yet; it waits on the owner's review.
	mcpUpstreamProxySentence = "MCP servers that start from a command can't run here yet: this organisation sends traffic through its own proxy. Use a remote (HTTPS) MCP server, or remove <server>."
	// mcpSandboxRunEndedSentence (S-ended) and mcpSandboxUnknownSentence (S-unknown) are the run page's answers.
	mcpSandboxRunEndedSentence = "This server stopped because the run's sandbox stopped."
	mcpSandboxUnknownSentence  = "This run has no MCP server called <server>."
	// mcpSandboxStartingSentence and mcpSandboxUnavailableSentence are the run page's Restart answers for a
	// server still starting and for a store that did not answer.
	// PROPOSED: these sentences are not in the approved strings yet; they wait on the owner's review.
	mcpSandboxStartingSentence    = "<server> is still starting. Restart it once it is ready."
	mcpSandboxUnavailableSentence = "The run's MCP servers could not be read just now. Try again."
	// mcpHostReachableSentence is the dispatch refusal on a runner whose MCP sandboxes would be reachable from
	// its host (runner.ErrMCPSandboxHostReachable).
	// PROPOSED: this sentence is not in the approved strings yet; it waits on the owner's review.
	mcpHostReachableSentence = "This runner can't keep <server> off its host: its Docker Engine is older than 28.0.0. Update Docker on the runner, or remove <server>."
	// mcpSaveOffSentence is the component save's refusal while the switch is off, in the validator's form.
	mcpSaveOffSentence = "mcp.transport: MCP servers that start from a command are turned off here."
	// mcpRunnerUnnamed is the unsupported-runner sentence's runner when the run's runner has no name to show.
	mcpRunnerUnnamed = "This runner"
)

// mcpSandboxSentence fills a sentence's <server> and <runner>.
func mcpSandboxSentence(template, server, runnerName string) string {
	return strings.NewReplacer("<server>", server, "<runner>", runnerName).Replace(template)
}
