// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// toolchainNeeds is dispatchParams.Toolchains' shape: which of the two
// toolchain-fidelity env groups this run's workspace scans actually detected.
type toolchainNeeds struct{ goTools, jvmTools bool }

// runToolchainNeeds derives a run's toolchainNeeds from its resolved
// workspaces: the union of every attached profile's detections
// (workspacescan.ToolchainNeeds — the same signals the devcontainer emission
// keys on). No workspaces at all, or ANY attached workspace lacking a
// decodable profile (unattached/unscanned/malformed) → nil (UNKNOWN; dispatch
// keeps the full set) — one unscanned workspace in an otherwise-scanned set
// must dominate the same way an all-unscanned set does, or its undeclared
// needs would silently ride on the scanned workspaces' narrower env. Only
// once EVERY workspace decodes a profile does empty needs apply: a workspace
// run's env then states what its requirements ground, nothing more.
func runToolchainNeeds(wsRefs []types.Workspace) *toolchainNeeds {
	profiles := make([]workspacescan.WorkspaceProfile, 0, len(wsRefs))
	for _, ws := range wsRefs {
		if p, ok := workspaceProfile(ws); ok {
			profiles = append(profiles, p)
		}
	}
	if len(wsRefs) == 0 || len(profiles) != len(wsRefs) {
		return nil
	}
	goNeeded, jvmNeeded := workspacescan.ToolchainNeeds(profiles...)
	return &toolchainNeeds{goTools: goNeeded, jvmTools: jvmNeeded}
}
