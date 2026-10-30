// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import "io/fs"

// The macOS guest agent's install, shared by the agent's own -plist default
// and the helper's stage 1, which installs it (MAC-VM §3.4 step 7a).
const (
	// MacAgentLabel is the agent's LaunchDaemon label.
	MacAgentLabel = "com.wardyn.guest-agent"
	// MacAgentPlistPath is the agent's LaunchDaemon plist.
	MacAgentPlistPath = "/Library/LaunchDaemons/" + MacAgentLabel + ".plist"
	// MacAgentBinaryPath is where stage 1 installs the agent's binary.
	MacAgentBinaryPath = "/usr/local/libexec/wardyn-guest-agent"
	// MacAgentRunPath is the run launcher every sandbox has (deploy/images/README.md): in a macOS guest,
	// the agent's own binary, which stage 2 pushes here and which runs AgentRunMain by this name.
	MacAgentRunPath = "/usr/local/bin/agent-run"
)

// Where a macOS guest's harnesses read the files a run's walls are made of. The runner's path table
// (internal/runner/docker/macvm_guestpaths.go) maps each Linux path the dispatcher names to one of
// these; a file it cannot map is refused, never written where no harness reads it.
const (
	// MacClaudeCodeManagedDir is where Claude Code reads managed-settings.json and managed-mcp.json on
	// macOS: "/Library/Application Support/ClaudeCode/ on macOS, /etc/claude-code/ on Linux and WSL"
	// (https://code.claude.com/docs/en/managed-settings, "Delivery mechanisms"). Confirmed on a real
	// Mac: pending.
	MacClaudeCodeManagedDir = "/Library/Application Support/ClaudeCode"
	// MacClaudeCodeManagedSettingsPath holds a run's autonomy ceiling and tool rules.
	MacClaudeCodeManagedSettingsPath = MacClaudeCodeManagedDir + "/managed-settings.json"
	// MacClaudeCodeManagedMCPPath holds an interactive run's MCP servers.
	MacClaudeCodeManagedMCPPath = MacClaudeCodeManagedDir + "/managed-mcp.json"
	// MacClaudeMCPConfigPath is the MCP file the launcher passes an autonomous Claude Code run. Claude
	// Code does not read it on its own; it sits beside the managed files so it is root's like them.
	MacClaudeMCPConfigPath = MacClaudeCodeManagedDir + "/wardyn-mcp.json"
	// MacCodexManagedDir is where Codex reads requirements.toml on macOS: "/etc/codex/requirements.toml
	// on Unix systems, including Linux and macOS"
	// (https://developers.openai.com/codex/enterprise/managed-configuration, "Locations"). Confirmed
	// on a real Mac: pending.
	MacCodexManagedDir = "/etc/codex"
	// MacCodexRequirementsPath binds Codex to a run's sandbox mode.
	MacCodexRequirementsPath = MacCodexManagedDir + "/requirements.toml"
	// MacRunDir is the guest's counterpart of the Linux /run/wardyn, where a delivered secret lands: a
	// macOS guest has no /run (its system volume is sealed).
	MacRunDir = "/var/run/wardyn"
)

// EnvManagedFiles names the managed files a run's Init wrote, for the launcher's check that each is in
// place before the run starts: a JSON list of ManagedFileCheck.
const EnvManagedFiles = "WARDYN_MANAGED_FILES"

// ManagedFileCheck is one managed file as Init wrote it: root's at Mode, or the agent's (a delivered
// secret).
type ManagedFileCheck struct {
	Path       string      `json:"path"`
	Mode       fs.FileMode `json:"mode"`
	AgentOwned bool        `json:"agent_owned,omitempty"`
}
