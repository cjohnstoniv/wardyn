// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import "github.com/cjohnstoniv/wardyn/internal/types"

// The broker's own defaults for push_rules (internal/egress/proxy push_rules.go
// and push_hold.go). The oracle tests in internal/egress/proxy pin both.
const (
	defaultInspectPackMiB = 32
	defaultPushHoldSec    = 120
)

// normPushHold is what push_rules.hold_seconds means at runtime: <= 0 is the
// default, and a larger value is cut to the proxy's hold ceiling.
func normPushHold(x int) int {
	if x <= 0 {
		return defaultPushHoldSec
	}
	return min(x, maxHoldSec)
}

// meetPush composes push_rules. Path rules only ever add (a union: more paths
// refused or held), and each bound takes the smaller value after the broker's
// own default is applied. A pack cap of 0 means 32 MiB under an ACTIVE rule and
// means nothing when no rule is active, so the two sides are normalised against
// whether the composed result inspects pushes at all.
func (m *meeter) meetPush(o *types.PushRulesOverlay) {
	if o == nil {
		return
	}
	c := &m.out.Ceiling
	var res types.PushRulesSpec
	if c.PushRules != nil {
		res = *c.PushRules
	}
	activeBase := res.IsSet()
	if o.DenyPaths != nil {
		res.DenyPaths = unionPaths(res.DenyPaths, *o.DenyPaths)
	}
	if o.RequireReviewPaths != nil {
		res.RequireReviewPaths = unionPaths(res.RequireReviewPaths, *o.RequireReviewPaths)
	}
	if o.DenyNewExecutables != nil {
		res.DenyNewExecutables = res.DenyNewExecutables || *o.DenyNewExecutables
	}
	if o.MaxFileSizeMiB != nil {
		if looser(*o.MaxFileSizeMiB, res.MaxFileSizeMiB) {
			m.widen("push_rules.max_file_size_mib", "%d is larger than the base's %d", *o.MaxFileSizeMiB, res.MaxFileSizeMiB)
		}
		res.MaxFileSizeMiB = tightest(res.MaxFileSizeMiB, *o.MaxFileSizeMiB)
	}
	if o.HoldSeconds != nil {
		b, v := normPushHold(res.HoldSeconds), normPushHold(*o.HoldSeconds)
		if v > b {
			m.widen("push_rules.hold_seconds", "%ds is longer than the base's %ds", v, b)
		}
		res.HoldSeconds = min(b, v)
	}
	active := res.IsSet() || (o.MaxInspectPackMiB != nil && *o.MaxInspectPackMiB > 0)
	if o.MaxInspectPackMiB != nil {
		m.meetPack(&res, *o.MaxInspectPackMiB, activeBase, active)
	}
	if active && res.MaxInspectPackMiB <= 0 {
		res.MaxInspectPackMiB = defaultInspectPackMiB
	}
	if res.IsSet() || res.HoldSeconds > 0 || res.DenyNewExecutables {
		c.PushRules = &res
		return
	}
	c.PushRules = nil
}

// meetPack meets the pack cap. A base without an active rule imposes none, so
// any overlay cap narrows it; a zero cap on the overlay is the broker's default
// whenever the composed rule is active.
func (m *meeter) meetPack(res *types.PushRulesSpec, v int, activeBase, active bool) {
	b := 0 // unbounded
	if activeBase {
		b = defaultInspectPackMiB
		if res.MaxInspectPackMiB > 0 {
			b = res.MaxInspectPackMiB
		}
	}
	if v <= 0 && active {
		v = defaultInspectPackMiB
	}
	if looser(v, b) {
		m.widen("push_rules.max_inspect_pack_mib", "%d is larger than the base's %d", v, b)
	}
	res.MaxInspectPackMiB = tightest(b, v)
}
