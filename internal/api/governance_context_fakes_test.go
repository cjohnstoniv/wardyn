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
