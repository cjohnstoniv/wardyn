// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"slices"
	"time"

	"github.com/google/uuid"
)

// MaxEnvelopeGroups is the most IdP groups one envelope may approve.
const MaxEnvelopeGroups = 64

// GovernanceEnvelope names one unit, the root of a platform, and the IdP groups security approved for
// it (table governance_envelopes). It changes only who must approve a write below the root: a unit
// admin's edits strictly inside the envelope, and the add or move of an approved group, apply
// directly. Nothing is enforced at run time. An envelope is set by the security tier alone, at most
// one covers any unit, and a group sits on at most one envelope.
type GovernanceEnvelope struct {
	UnitID         uuid.UUID `json:"unit_id"`
	ApprovedGroups []string  `json:"approved_groups"`
	SetBy          string    `json:"set_by"`
	SetAt          time.Time `json:"set_at"`
}

// ValidateEnvelope canonicalizes the approved groups of an envelope (CanonicalGroupSubject) and returns
// them sorted, never nil. It refuses an unusable group name, a duplicate once folded, and more than
// MaxEnvelopeGroups. An empty list is valid: the envelope then only widens what a unit admin applies
// directly, and approves no group.
func ValidateEnvelope(groups []string) ([]string, error) {
	if len(groups) > MaxEnvelopeGroups {
		return nil, fmt.Errorf("approved_groups: %d groups, at most %d", len(groups), MaxEnvelopeGroups)
	}
	out := make([]string, 0, len(groups))
	for _, g := range groups {
		c, ok := CanonicalGroupSubject(g)
		if !ok {
			return nil, fmt.Errorf("approved_groups: %q is not a group name", g)
		}
		if slices.Contains(out, c) {
			return nil, fmt.Errorf("approved_groups: %q is listed twice", c)
		}
		out = append(out, c)
	}
	slices.Sort(out)
	return out, nil
}
