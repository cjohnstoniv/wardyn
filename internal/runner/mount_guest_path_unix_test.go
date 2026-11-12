// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package runner

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/vm/guestagent"
)

// TestMacGuestPathHomesMatchTheGuestAgent: mount.go writes the two homes out so
// the package builds for Windows; they stay the guest agent's own.
func TestMacGuestPathHomesMatchTheGuestAgent(t *testing.T) {
	if agentHome != guestagent.AgentHome || macAgentHome != guestagent.MacAgentHome {
		t.Errorf("homes = %q, %q; the guest agent's are %q, %q", agentHome, macAgentHome, guestagent.AgentHome, guestagent.MacAgentHome)
	}
}
