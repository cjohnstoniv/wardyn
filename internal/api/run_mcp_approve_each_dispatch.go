// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mcpApproveEachMarker is the proxy config's approve_each marker: present exactly when a rule the
// proxy carries is approve_each, nil otherwise. A proxy image older than the effect refuses the
// key, so such a run fails at start instead of forwarding the calls unasked; every other run
// writes no key and starts on it as before. The broker picks each harness's window.
func mcpApproveEachMarker(policy types.RunPolicySpec, sets []types.HarnessToolRules) *proxy.MCPApproveEachConfig {
	if !types.HasApproveEach(policy.ToolRules, sets) {
		return nil
	}
	return &proxy.MCPApproveEachConfig{}
}
