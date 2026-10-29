// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// PersonalWallsPrefKey is the principal_prefs key a person's walls are stored under.
const PersonalWallsPrefKey = "personal_walls"

// PersonalWalls is a person's own walls: one private document, narrow-only, that is met into every run
// they start. It is not a ceiling and no one else reads or writes it.
type PersonalWalls struct {
	Spec      PersonalWallsSpec `json:"spec"`
	UpdatedAt *time.Time        `json:"updated_at,omitempty"`
}

// PersonalWallsSpec is the part of a RunPolicySpec a person may wall off. Presence is typed, as in a
// composed unit's overlay: a knob it does not name is no limit. It can only narrow, so a bool is
// absent or false, a desktop direction is absent or blocked, and the lists can only shrink a run.
type PersonalWallsSpec struct {
	// AllowedDomains is "only these sites"; [] is only what the run's own setup needs.
	AllowedDomains      *[]string         `json:"allowed_domains,omitempty"`
	DeniedDomains       *[]string         `json:"denied_domains,omitempty"`
	AllowAllEgress      *bool             `json:"allow_all_egress,omitempty"`
	FirstUseApproval    *FirstUseMode     `json:"first_use_approval,omitempty"`
	MinConfinementClass *ConfinementClass `json:"min_confinement_class,omitempty"`
	Desktop             *DesktopControls  `json:"desktop,omitempty"`
	// WithheldGrants names credentials the run is not given, in the restriction's shape: a grant of the
	// same kind (a scope-less entry) or the same pairing. It never reaches Overlay().
	WithheldGrants []GrantSpec `json:"withheld_grants,omitempty"`
}

// Empty reports whether the walls name nothing, so no run is narrowed by them. A desktop block that
// names no direction is nothing.
func (s PersonalWallsSpec) Empty() bool {
	return s.AllowedDomains == nil && s.DeniedDomains == nil && s.AllowAllEgress == nil &&
		s.FirstUseApproval == nil && s.MinConfinementClass == nil &&
		(s.Desktop == nil || *s.Desktop == DesktopControls{}) && len(s.WithheldGrants) == 0
}

// Overlay is the spec as a ceiling overlay. As the boundary's does, an allowlist with allow_all_egress
// unnamed also turns open egress off, since an allowlist bounds nothing while all egress is open. A
// desktop direction the walls do not name is allowed, so the walls block only what they name (an unset
// direction would otherwise read as its default, which blocks copy_out and download).
func (s PersonalWallsSpec) Overlay() CeilingOverlay {
	out := CeilingOverlay{
		AllowedDomains: s.AllowedDomains, DeniedDomains: s.DeniedDomains, AllowAllEgress: s.AllowAllEgress,
		FirstUseApproval: s.FirstUseApproval, MinConfinementClass: s.MinConfinementClass,
	}
	if s.AllowedDomains != nil && s.AllowAllEgress == nil {
		off := false
		out.AllowAllEgress = &off
	}
	if s.Desktop != nil {
		d := DesktopControls{PasteIn: TransferAllowed, CopyOut: TransferAllowed, Upload: TransferAllowed, Download: TransferAllowed}
		for _, dir := range []struct{ walls, out *TransferRule }{
			{&s.Desktop.PasteIn, &d.PasteIn}, {&s.Desktop.CopyOut, &d.CopyOut},
			{&s.Desktop.Upload, &d.Upload}, {&s.Desktop.Download, &d.Download},
		} {
			if *dir.walls == TransferBlocked {
				*dir.out = TransferBlocked
			}
		}
		out.Desktop = &d
	}
	return out
}
