// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import "github.com/cjohnstoniv/wardyn/internal/types"

// meetHostRules appends the overlay's host rule sets to the base's (types.AppendHostRuleSets): a set
// only ever refuses more, so nothing is a widening, and no rule is merged across layers. More sets than
// a policy may hold has no representable result.
func (m *meeter) meetHostRules(o *[]types.HostRuleSet) {
	if o == nil {
		return
	}
	c := &m.out.Ceiling
	c.HostRules = types.AppendHostRuleSets(c.HostRules, *o)
	if len(c.HostRules) > types.MaxHostRuleSets {
		m.fail(ReasonOverlayUnsatisfiable, "host_rules", "%d rule sets after composition, at most %d", len(c.HostRules), types.MaxHostRuleSets)
	}
}
