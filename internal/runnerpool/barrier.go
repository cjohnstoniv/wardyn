// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// BarrierReason says why a barrier is not available for a run in a pool. The set is closed;
// runner-pool-refusals.ts repeats it as BARRIER_REASON. When several apply, the first in this order
// wins.
type BarrierReason string

const (
	// BarrierBelowFloor: below the caller's governance floor.
	BarrierBelowFloor BarrierReason = "below_floor"
	// BarrierPolicyFloor: at or above the governance floor but below this run's policy floor (Review only).
	BarrierPolicyFloor BarrierReason = "policy_floor"
	// BarrierAccessFloor: this run's access needs Vault (composer.RequiredConfinementFloor; Review only).
	BarrierAccessFloor BarrierReason = "access_floor"
	// BarrierPoolNotAllowed: the pool's own limits do not allow it (CheckBarrier).
	BarrierPoolNotAllowed BarrierReason = "pool_not_allowed"
	// BarrierUnknown: no candidate's facts are known (every own runner offline, or the executor read failed).
	BarrierUnknown BarrierReason = "unknown"
	// BarrierNotOffered: some candidate is known and none enforces it.
	BarrierNotOffered BarrierReason = "not_offered"
)

// BarrierReasons lists the closed set in precedence order.
func BarrierReasons() []BarrierReason {
	return []BarrierReason{BarrierBelowFloor, BarrierPolicyFloor, BarrierAccessFloor, BarrierPoolNotAllowed, BarrierUnknown, BarrierNotOffered}
}

// BarrierDetailKVMMissing is the one reason detail: Vault is not offered because this deployment's
// Docker host has no KVM.
const BarrierDetailKVMMissing = "kvm_missing"

// BarrierDefaultSource says who chose the default barrier.
type BarrierDefaultSource string

const (
	// BarrierDefaultServer: the strongest class offered at or above the floor.
	BarrierDefaultServer BarrierDefaultSource = "server"
	// BarrierDefaultOrganisation: the organisation's default barrier, at or above the floor and offered.
	BarrierDefaultOrganisation BarrierDefaultSource = "organisation"
)

// BarrierOption is one barrier as a pool offers it to the caller: always CC1, CC2 and CC3, in that order.
type BarrierOption struct {
	Class         types.ConfinementClass `json:"class"`
	Available     bool                   `json:"available"`
	Default       bool                   `json:"default,omitempty"`
	DefaultSource BarrierDefaultSource   `json:"default_source,omitempty"`
	ReasonCode    BarrierReason          `json:"reason_code,omitempty"`
	ReasonDetail  string                 `json:"reason_detail,omitempty"`
	// Floor is the class a *_floor reason names.
	Floor types.ConfinementClass `json:"floor,omitempty"`
}

// BarrierFacts is what one pool's barriers are judged on for one caller.
type BarrierFacts struct {
	// Limits are the pool's own; nil allows every barrier.
	Limits *types.RunnerPoolLimits
	// Known: some candidate's facts are known. Offered is the union of the known candidates' classes.
	Known   bool
	Offered []types.ConfinementClass
	// Floor is the governance floor. PolicyFloor and AccessFloor are a draft's (Review); zero on the list.
	Floor, PolicyFloor types.ConfinementClass
	AccessFloor        bool
	// Org is the organisation's default barrier; zero when it sets none.
	Org types.ConfinementClass
	// KVMMissing: Vault is not offered because the deployment's Docker host has no KVM.
	KVMMissing bool
}

// BarrierOptions is the three barriers for one pool. The default is the class an untouched run gets:
// the organisation's when it is available, else the strongest available one (the strongest offered at
// or above every floor).
func BarrierOptions(f BarrierFacts) []BarrierOption {
	out := make([]BarrierOption, 0, 3)
	for _, c := range []types.ConfinementClass{types.CC1, types.CC2, types.CC3} {
		o := BarrierOption{Class: c}
		o.ReasonCode, o.Floor = f.reason(c)
		o.Available = o.ReasonCode == ""
		if o.ReasonCode == BarrierNotOffered && c == types.CC3 && f.KVMMissing {
			o.ReasonDetail = BarrierDetailKVMMissing
		}
		out = append(out, o)
	}
	if i := slices.IndexFunc(out, func(o BarrierOption) bool { return o.Class == f.Org && o.Available }); i >= 0 {
		out[i].Default, out[i].DefaultSource = true, BarrierDefaultOrganisation
		return out
	}
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Available {
			out[i].Default, out[i].DefaultSource = true, BarrierDefaultServer
			break
		}
	}
	return out
}

func (f BarrierFacts) reason(c types.ConfinementClass) (BarrierReason, types.ConfinementClass) {
	below := func(floor types.ConfinementClass) bool { return floor != "" && c.Rank() < floor.Rank() }
	switch {
	case below(f.Floor):
		return BarrierBelowFloor, f.Floor
	case below(f.PolicyFloor):
		return BarrierPolicyFloor, f.PolicyFloor
	case f.AccessFloor && below(types.CC3):
		return BarrierAccessFloor, types.CC3
	case f.Limits != nil && !f.Limits.AllowsBarrier(c):
		return BarrierPoolNotAllowed, ""
	case !f.Known:
		return BarrierUnknown, ""
	case !slices.Contains(f.Offered, c):
		return BarrierNotOffered, ""
	}
	return "", ""
}
