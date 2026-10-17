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
// block. The two switches are plain booleans; only autonomy_cap has a closed set.
func validateComponentSettings(cs *types.ComponentSettings) error {
	if cs == nil {
		return nil
	}
	switch cs.AutonomyCap {
	case "", types.AutonomyL1, types.AutonomyL0:
		return nil
	}
	return fmt.Errorf(`components.autonomy_cap: %q is not accepted — use "" (no cap), "L1" (hold tool calls) or "L0" (refuse unattended runs); the cap can only tighten`, cs.AutonomyCap)
}

// auditComponentSettings adds the components block to a site_config.write datum,
// so lifting a cap or the Vault floor is reviewable from the log alone. Only once
// a block is stored or the body named one, so a deployment with none writes the
// row it always wrote; a cleared block records the defaults it now reads as.
func auditComponentSettings(datum map[string]any, cs *types.ComponentSettings, named bool) {
	if cs == nil && !named {
		return
	}
	eff := componentSettings(types.SiteConfig{Components: cs})
	datum["components_autonomy_cap"] = string(eff.AutonomyCap)
	datum["components_deny_resident_delivery"] = eff.DenyResidentDelivery
	datum["components_require_vault"] = eff.RequireVaultForCredentials
}

// carryForwardComponentSettings keeps the stored components block when the body
// did not mention it, for the MDM re-apply reason the provider blocks state, and
// stores an all-default block as none so a GET stays byte-identical to an install
// that never set one.
func carryForwardComponentSettings(cfg *types.SiteConfig, existing types.SiteConfig, present map[string]bool) {
	if !present["components"] {
		cfg.Components = existing.Components
	} else if cfg.Components != nil && *cfg.Components == (types.ComponentSettings{}) {
		cfg.Components = nil
	}
}
