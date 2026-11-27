// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// componentSettings is the one read of the site config's components block. A nil
// block — never set, or a document stored before the block existed — reads as the
// zero value, which is the owner-decided default for every field.
func componentSettings(sc types.SiteConfig) types.ComponentSettings {
	if sc.Components == nil {
		return types.ComponentSettings{}
	}
	return *sc.Components
}

// validateComponentSettings is PUT /site-config's shape check for the components
// block. The two switches are plain booleans; autonomy_cap and local_mcp_servers
// each have a closed set.
func validateComponentSettings(cs *types.ComponentSettings) error {
	if cs == nil {
		return nil
	}
	switch cs.AutonomyCap {
	case "", types.AutonomyL1, types.AutonomyL0:
	default:
		return fmt.Errorf(`components.autonomy_cap: %q is not accepted — use "" (no cap), "L1" (hold tool calls) or "L0" (refuse unattended runs); the cap can only tighten`, cs.AutonomyCap)
	}
	if v := cs.LocalMCPServers; v != nil && *v != "" && *v != types.LocalMCPOff && *v != types.LocalMCPOn {
		return fmt.Errorf(`components.local_mcp_servers: %q is not accepted — use "off" or "on"; unset reads on`, *v)
	}
	return nil
}

// localMCPOn is the organisation's Local MCP servers setting: on unless it is
// stored as off.
func localMCPOn(sc types.SiteConfig) bool {
	v := componentSettings(sc).LocalMCPServers
	return v == nil || *v != types.LocalMCPOff
}

// localMCPOnRead is localMCPOn on a site config that was read (ok); one that
// could not be read reads as off.
func localMCPOnRead(sc types.SiteConfig, ok bool) bool { return ok && localMCPOn(sc) }

// auditComponentSettings adds the components block to a site_config.write datum,
// so lifting a cap or the Vault floor is reviewable from the log alone. Only once
// a block is stored or the body named one, so a deployment with none writes the
// row it always wrote; a cleared block records the defaults it now reads as.
func auditComponentSettings(datum map[string]any, cs *types.ComponentSettings, named bool) {
	if cs == nil && !named {
		return
	}
	sc := types.SiteConfig{Components: cs}
	eff := componentSettings(sc)
	datum["components_autonomy_cap"] = string(eff.AutonomyCap)
	datum["components_deny_resident_delivery"] = eff.DenyResidentDelivery
	datum["components_require_vault"] = eff.RequireVaultForCredentials
	datum["components_local_mcp_servers"] = string(types.LocalMCPOff)
	if localMCPOn(sc) {
		datum["components_local_mcp_servers"] = string(types.LocalMCPOn)
	}
}

// carryForwardComponentSettings keeps the stored components block when the body
// did not mention it, for the MDM re-apply reason the provider blocks state, and
// stores an all-default block as none so a GET stays byte-identical to an install
// that never set one. A body that names the block but not local_mcp_servers (an
// older console tab or managed file; "components": {} and null included) keeps
// the stored setting: only an explicit value changes it, and "" is stored as none.
func carryForwardComponentSettings(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["components"] {
		cfg.Components = existing.Components
		return
	}
	if stored := componentSettings(existing).LocalMCPServers; stored != nil && (cfg.Components == nil || cfg.Components.LocalMCPServers == nil) {
		if cfg.Components == nil {
			cfg.Components = &types.ComponentSettings{}
		}
		v := *stored
		cfg.Components.LocalMCPServers = &v
	}
	if cfg.Components == nil {
		return
	}
	if v := cfg.Components.LocalMCPServers; v != nil && *v == "" {
		cfg.Components.LocalMCPServers = nil
	}
	if *cfg.Components == (types.ComponentSettings{}) {
		cfg.Components = nil
	}
}
