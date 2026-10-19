// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
)

// BoundaryMode is the Wardyn boundary's mode: no_limits matches the organisation's boundary (the
// limits outside Wardyn, which Wardyn never sees), so it narrows nothing; hard_limits is a Wardyn
// allow/deny list inside it.
type BoundaryMode string

const (
	BoundaryNoLimits   BoundaryMode = "no_limits"
	BoundaryHardLimits BoundaryMode = "hard_limits"
)

// Boundary is the Wardyn boundary (migration 0196): the outermost layer of every run's effective
// policy. A deployment with no row is in no_limits, so every run resolves as it did before the
// boundary existed.
//
// Presence is typed, as in a composed profile's overlay: a knob the boundary does not name is no
// limit. The spec is kept in no_limits too, as a draft that binds nothing.
type Boundary struct {
	Mode      BoundaryMode       `json:"mode"`
	Spec      BoundarySpec       `json:"spec"`
	Limits    BoundaryLimits     `json:"limits"`
	Contact   *policyref.Contact `json:"contact,omitempty"`
	UpdatedBy string             `json:"updated_by,omitempty"`
	UpdatedAt *time.Time         `json:"updated_at,omitempty"`
}

// HardLimits reports whether the boundary binds runs.
func (b Boundary) HardLimits() bool { return b.Mode == BoundaryHardLimits }

// BoundarySpec is the part of a RunPolicySpec the boundary bounds. Each knob meets without a state
// that nothing satisfies, so the boundary narrows a wider policy and never refuses it.
type BoundarySpec struct {
	AllowedDomains      *[]string         `json:"allowed_domains,omitempty"`
	DeniedDomains       *[]string         `json:"denied_domains,omitempty"`
	AllowAllEgress      *bool             `json:"allow_all_egress,omitempty"`
	MinConfinementClass *ConfinementClass `json:"min_confinement_class,omitempty"`
	EligibleGrants      *[]GrantSpec      `json:"eligible_grants,omitempty"`
}

// BoundaryLimits is the GovernanceLimits subset the boundary bounds. MaxEndAheadSec is also the cap
// on a run governance alone leaves uncapped (an operator's, or the admin token's).
type BoundaryLimits struct {
	MaxEndAheadSec *int `json:"max_end_ahead_sec,omitempty"`
}

// BoundaryWrite is the body of PUT /governance/boundary: the whole document but the server-set
// fields. Mode is required, so a write that only edits the spec cannot change the mode by leaving it
// out.
type BoundaryWrite struct {
	Mode    BoundaryMode       `json:"mode"`
	Spec    BoundarySpec       `json:"spec"`
	Limits  BoundaryLimits     `json:"limits"`
	Contact *policyref.Contact `json:"contact,omitempty"`
}

// Overlay is the boundary as a composed-profile overlay over an effective policy. Eligible grants
// are left out: an overlay's grants pass through the meet, and the boundary re-intersects them
// instead. A boundary allowlist with allow_all_egress unnamed also turns open egress off, since an
// allowlist bounds nothing while all egress is open.
func (b BoundarySpec) Overlay() CeilingOverlay {
	out := CeilingOverlay{
		AllowedDomains: b.AllowedDomains, DeniedDomains: b.DeniedDomains,
		AllowAllEgress: b.AllowAllEgress, MinConfinementClass: b.MinConfinementClass,
	}
	if b.AllowedDomains != nil && b.AllowAllEgress == nil {
		off := false
		out.AllowAllEgress = &off
	}
	return out
}

// Overlay is the boundary's limits as a limits overlay.
func (l BoundaryLimits) Overlay() LimitsOverlay {
	return LimitsOverlay{MaxEndAheadSec: l.MaxEndAheadSec}
}
