// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/cjohnstoniv/wardyn/internal/adoscope"
	"github.com/cjohnstoniv/wardyn/internal/ghscope"
)

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
	// AzureDevOpsCapabilities and GitHubCapabilities are a subset of what the run's own policy names
	// (meet = intersection). They cannot be proven within a provider row's default profile, which a
	// policy that names no list falls back to, so they narrow only a run whose policy names a list.
	AzureDevOpsCapabilities *[]adoscope.Capability `json:"azure_devops_capabilities,omitempty"`
	GitHubCapabilities      *[]ghscope.Capability  `json:"github_capabilities,omitempty"`
	// ToolRules are rules ADDED to the run's: each holds or denies a tool, never allows one, and never
	// loosens a rule the run already has. It never reaches Overlay(): the meet spells out the "*"
	// default too, which would tighten every tool the person did not name, so the run step adds the
	// rules one by one.
	ToolRules *[]ToolRule `json:"tool_rules,omitempty"`
	// PushRules adds paths to the run's push deny list (a union, so only more is refused).
	PushRules *PersonalWallsPushRules `json:"push_rules,omitempty"`
	// Recording is a floor: the run is recorded at least at this level (meet = AtLeast). Only terminal
	// and screen_actions name one; off is no floor.
	Recording *RecordingLevel `json:"recording,omitempty"`
	// AutoStopAfterSec is a cap on the idle seconds before a sandbox is stopped (meet = the smaller),
	// always positive: zero and below mean "never stop" in a policy, which is no cap.
	AutoStopAfterSec *int `json:"auto_stop_after_sec,omitempty"`
}

// PersonalWallsPushRules is the one part of a policy's push_rules a person may add to: paths a
// brokered push may not touch. It is PushRulesOverlay's deny_paths and nothing else of it.
type PersonalWallsPushRules struct {
	DenyPaths *[]string `json:"deny_paths,omitempty"`
}

// Empty reports whether the walls name nothing, so no run is narrowed by them. A desktop block that
// names no direction is nothing.
func (s PersonalWallsSpec) Empty() bool {
	return s.AllowedDomains == nil && s.DeniedDomains == nil && s.AllowAllEgress == nil &&
		s.FirstUseApproval == nil && s.MinConfinementClass == nil &&
		(s.Desktop == nil || *s.Desktop == DesktopControls{}) && len(s.WithheldGrants) == 0 &&
		s.AzureDevOpsCapabilities == nil && s.GitHubCapabilities == nil &&
		(s.ToolRules == nil || len(*s.ToolRules) == 0) && s.AddedDenyPaths() == nil &&
		s.Recording == nil && s.AutoStopAfterSec == nil
}

// AddedDenyPaths is the push deny list the walls add, nil when they add none.
func (s PersonalWallsSpec) AddedDenyPaths() *[]string {
	if s.PushRules == nil || s.PushRules.DenyPaths == nil || len(*s.PushRules.DenyPaths) == 0 {
		return nil
	}
	return s.PushRules.DenyPaths
}

// Overlay is the spec as a ceiling overlay. As the boundary's does, an allowlist with allow_all_egress
// unnamed also turns open egress off, since an allowlist bounds nothing while all egress is open. A
// desktop direction the walls do not name is allowed, so the walls block only what they name (an unset
// direction would otherwise read as its default, which blocks copy_out and download).
func (s PersonalWallsSpec) Overlay() CeilingOverlay {
	out := CeilingOverlay{
		AllowedDomains: s.AllowedDomains, DeniedDomains: s.DeniedDomains, AllowAllEgress: s.AllowAllEgress,
		FirstUseApproval: s.FirstUseApproval, MinConfinementClass: s.MinConfinementClass,
		AzureDevOpsCapabilities: s.AzureDevOpsCapabilities, GitHubCapabilities: s.GitHubCapabilities,
		Recording: s.Recording, AutoStopAfterSec: s.AutoStopAfterSec,
	}
	if d := s.AddedDenyPaths(); d != nil {
		out.PushRules = &PushRulesOverlay{DenyPaths: d}
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
