// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import "github.com/cjohnstoniv/wardyn/internal/types"

// meetDataOut meets the run's data_out rule with an overlay's: the stricter of the two (allowed < review <
// blocked, "" read as allowed). An overlay looser than the base is a widening.
func (m *meeter) meetDataOut(o *types.TransferRule) {
	if o == nil {
		return
	}
	c := &m.out.Ceiling
	if !leqDataOut(*o, c.DataOut) {
		m.widen("data_out", "the base holds content in, and an overlay can only hold in more")
	}
	c.DataOut = types.StricterTransfer(c.DataOut.DataOut(), o.DataOut())
}

// impliedByDataOut turns a blocked data_out into the denies that keep content in by the other doors: no
// user drive, no member drives, no kept machines, and no git push. Review implies none of them. The drive and
// kept-machine denies bind only where those doors already apply a ceiling's limits.
func (m *meeter) impliedByDataOut() {
	if m.out.Ceiling.DataOut.DataOut() != types.TransferBlocked {
		return
	}
	l := &m.out.Limits
	l.DenyUserDrive, l.DenyMemberDrives, l.DenyKeptMachines = true, true, true
	m.out.Ceiling.PushRules = pushDenyAll(m.out.Ceiling.PushRules)
}

// pushDenyAll is p with deny_all set, on a copy.
func pushDenyAll(p *types.PushRulesSpec) *types.PushRulesSpec {
	var out types.PushRulesSpec
	if p != nil {
		out = *p
	}
	out.DenyAll = true
	return &out
}

// leqDataOut holds when a is at least as strict as b (allowed < review < blocked, "" read as allowed).
func leqDataOut(a, b types.TransferRule) bool {
	return types.TransferStrictness(a.DataOut()) >= types.TransferStrictness(b.DataOut())
}
