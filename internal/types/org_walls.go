// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// OrgWalls is the site-level organisation walls document (migration 0196): the outermost limit on
// every run's effective policy, super admins' own runs included. Only the admin token's break-glass
// run stands outside it. Enabled is false until a security admin turns it on, and a deployment with
// no row has no walls, so either way every run resolves as it did before walls existed.
//
// Presence is typed, as in a composed profile's overlay: a knob the walls do not name is no wall.
type OrgWalls struct {
	Enabled   bool           `json:"enabled"`
	Spec      OrgWallsSpec   `json:"spec"`
	Limits    OrgWallsLimits `json:"limits"`
	UpdatedBy string         `json:"updated_by,omitempty"`
	UpdatedAt *time.Time     `json:"updated_at,omitempty"`
}

// OrgWallsSpec is the part of a RunPolicySpec the walls bound. Each knob meets without a state that
// nothing satisfies, so the walls narrow a wider policy and never refuse it.
type OrgWallsSpec struct {
	AllowedDomains      *[]string         `json:"allowed_domains,omitempty"`
	DeniedDomains       *[]string         `json:"denied_domains,omitempty"`
	AllowAllEgress      *bool             `json:"allow_all_egress,omitempty"`
	MinConfinementClass *ConfinementClass `json:"min_confinement_class,omitempty"`
	EligibleGrants      *[]GrantSpec      `json:"eligible_grants,omitempty"`
}

// OrgWallsLimits is the GovernanceLimits subset the walls bound. MaxEndAheadSec is also the cap on a
// break-glass run.
type OrgWallsLimits struct {
	MaxEndAheadSec *int `json:"max_end_ahead_sec,omitempty"`
}

// OrgWallsWrite is the body of PUT /governance/walls: the whole document but the server-set fields.
type OrgWallsWrite struct {
	Enabled bool           `json:"enabled"`
	Spec    OrgWallsSpec   `json:"spec"`
	Limits  OrgWallsLimits `json:"limits"`
}

// Overlay is the walls as a composed-profile overlay over an effective policy. Eligible grants are
// left out: an overlay's grants pass through the meet, and the walls re-intersect them instead. A
// walls allowlist with allow_all_egress unnamed also turns open egress off, since an allowlist
// bounds nothing while all egress is open.
func (w OrgWallsSpec) Overlay() CeilingOverlay {
	out := CeilingOverlay{
		AllowedDomains: w.AllowedDomains, DeniedDomains: w.DeniedDomains,
		AllowAllEgress: w.AllowAllEgress, MinConfinementClass: w.MinConfinementClass,
	}
	if w.AllowedDomains != nil && w.AllowAllEgress == nil {
		off := false
		out.AllowAllEgress = &off
	}
	return out
}

// Overlay is the walls' limits as a limits overlay.
func (l OrgWallsLimits) Overlay() LimitsOverlay {
	return LimitsOverlay{MaxEndAheadSec: l.MaxEndAheadSec}
}
