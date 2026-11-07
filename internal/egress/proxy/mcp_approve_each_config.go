// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MCPApproveEachConfig is the sealed-config marker that a run carries an approve_each tool rule
// (Config.MCPApproveEach). It is present exactly then, and that is the point: a proxy built
// before the effect refuses the unknown key (LoadConfigBytes decodes strictly), so the run fails
// at start. Without it, an older broker would read approve_each as an effect it does not know,
// forward every call that is not denied, and the control plane's per-call approval would turn
// into no approval at all. A current proxy refuses the opposite mismatch too: a rule with no
// key came from a writer that forgot it.
type MCPApproveEachConfig struct {
	// Windows is how long, in seconds, a call waits for a person on the open request, by harness
	// id. A harness with no entry gets the broker's default. Each is 1 to 300, under the idle
	// timer of the slowest client.
	Windows map[string]int `json:"windows,omitempty"`
}

func (c *MCPApproveEachConfig) validate() error {
	if c == nil {
		return nil
	}
	for harness, w := range c.Windows {
		if harness == "" || w < 1 || w > int(types.HoldWindowMCPCall.Seconds()) {
			return fmt.Errorf("config: mcp_approve_each.windows[%q] = %d; name a harness and give 1 to %d seconds", harness, w, int(types.HoldWindowMCPCall.Seconds()))
		}
	}
	return nil
}

// checkMCPApproveEach holds the key and the rules together: the key is present exactly when some
// dispatched rule is approve_each.
func (c *Config) checkMCPApproveEach() error {
	if err := c.MCPApproveEach.validate(); err != nil {
		return err
	}
	has := types.HasApproveEach(c.Policy.ToolRules, c.HarnessToolRules)
	switch {
	case has && c.MCPApproveEach == nil:
		return fmt.Errorf("config: a tool rule is approve_each and mcp_approve_each is absent; the config is written with both or neither")
	case !has && c.MCPApproveEach != nil:
		return fmt.Errorf("config: mcp_approve_each is set and no tool rule is approve_each; the config is written with both or neither")
	}
	return nil
}
