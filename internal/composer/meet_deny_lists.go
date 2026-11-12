// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The unit deny lists (GovernanceLimits.DenyComponents and DenyModelProviders) compose as a
// denial: the meet is the union, so deny wins, and Leq (leq.go) asks the result for a superset of
// what the base denies. Empty denies nothing, so a profile that leaves a list unset refuses no run.

// meetDenyLists unions an overlay's two deny lists into the composed limits.
func (m *meeter) meetDenyLists(o types.LimitsOverlay) {
	l := &m.out.Limits
	if o.DenyComponents != nil {
		l.DenyComponents = unionIDs(l.DenyComponents, *o.DenyComponents)
	}
	if o.DenyModelProviders != nil {
		l.DenyModelProviders = unionIDs(l.DenyModelProviders, *o.DenyModelProviders)
	}
}

// validateDenyLists refuses an overlay's deny list that a profile could not hold.
func (m *meeter) validateDenyLists(o types.LimitsOverlay) {
	for _, f := range []struct {
		field string
		list  *[]string
	}{{"deny_components", o.DenyComponents}, {"deny_model_providers", o.DenyModelProviders}} {
		if f.list == nil {
			continue
		}
		if err := types.ValidDenyIDs(*f.list); err != nil {
			m.fail(ReasonOverlayInvalid, f.field, "%v", err)
		}
	}
}

// unionIDs is a denial list only ever added to: base order first, then the overlay's new ids.
// A fresh slice, so the base is never aliased; nil when both are empty.
func unionIDs(base, ov []string) []string {
	var out []string
	for _, id := range slices.Concat(base, ov) {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}
