// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

// The macOS guest agent's install, shared by the agent's own -plist default
// and the helper's stage 1, which installs it (MAC-VM §3.4 step 7a).
const (
	// MacAgentLabel is the agent's LaunchDaemon label.
	MacAgentLabel = "com.wardyn.guest-agent"
	// MacAgentPlistPath is the agent's LaunchDaemon plist.
	MacAgentPlistPath = "/Library/LaunchDaemons/" + MacAgentLabel + ".plist"
	// MacAgentBinaryPath is where stage 1 installs the agent's binary.
	MacAgentBinaryPath = "/usr/local/libexec/wardyn-guest-agent"
)
