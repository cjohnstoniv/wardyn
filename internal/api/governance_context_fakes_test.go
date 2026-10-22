// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// membershipsFromResolve turns a fake's one-assignment answer (the shape before the policy tree)
// into the membership list the context resolver reads: the one row, legacy, at its tier. A legacy
// row is a context exactly as the one assignment was, so a fake keeps its meaning.
func membershipsFromResolve(p *types.GovernanceUnit, tier types.CapabilitySubjectType, err error) ([]store.MembershipMatch, error) {
	if errors.Is(err, store.ErrNotFound) || (err == nil && p == nil) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return []store.MembershipMatch{{
		Membership: types.GovernanceMembership{SubjectType: tier, UnitID: p.ID, Legacy: true},
		Unit:       *p,
	}}, nil
}

// allRowsFor is a fake's membership table for units it serves by id (the shape before the policy
// tree, where a run's unit was all a revive re-read): one `all` row per unit, so a revive or an
// extension under any of them keeps today's reach.
func allRowsFor(units []types.GovernanceUnit) []types.GovernanceMembership {
	out := make([]types.GovernanceMembership, 0, len(units))
	for _, u := range units {
		out = append(out, types.GovernanceMembership{SubjectType: types.CapabilitySubjectAll, UnitID: u.ID, Legacy: true})
	}
	return out
}
