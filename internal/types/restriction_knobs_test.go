// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"reflect"
	"testing"
)

// A restriction's six narrow-only knobs reach the overlay one for one, and a knob it does not name stays
// unnamed there.
func TestRestrictionOverlayNewKnobs(t *testing.T) {
	domains := []string{"a.example"}
	rules := []ToolRule{{Tool: "Bash", Effect: ToolHold}}
	deny := true
	push := &PushRulesOverlay{DenyAll: &deny}
	desk := &DesktopControls{CopyOut: TransferBlocked}
	rec := RecordingScreenActions
	models := []string{"model-a"}

	ov := RestrictionSpec{AllowedDomains: &domains, ToolRules: &rules, PushRules: push, Desktop: desk, Recording: &rec}.Overlay()
	if !reflect.DeepEqual(ov.AllowedDomains, &domains) || !reflect.DeepEqual(ov.ToolRules, &rules) || ov.PushRules != push ||
		ov.Desktop != desk || ov.Recording != &rec {
		t.Errorf("overlay = %+v, want the five knobs carried over", ov)
	}
	if lo := (RestrictionLimits{AllowedModels: &models}).Overlay(); !reflect.DeepEqual(lo.AllowedModels, &models) {
		t.Errorf("limits overlay allowed_models = %v, want %v", lo.AllowedModels, models)
	}

	empty := RestrictionSpec{}.Overlay()
	if empty.AllowedDomains != nil || empty.ToolRules != nil || empty.PushRules != nil || empty.Desktop != nil || empty.Recording != nil {
		t.Errorf("an empty restriction names %+v, want none of the five", empty)
	}
	if lo := (RestrictionLimits{}).Overlay(); lo.AllowedModels != nil {
		t.Errorf("an empty restriction's limits name allowed_models %v, want none", lo.AllowedModels)
	}
}

// An allowlist bounds nothing while all egress is open, so a restriction that names allowed_domains and not
// allow_all_egress closes open egress, as the boundary's does; one that names neither leaves it open.
func TestRestrictionAllowedDomainsClosesOpenEgress(t *testing.T) {
	off := false
	domains := []string{"a.example"}
	for _, tc := range []struct {
		name string
		spec RestrictionSpec
		want bool
	}{
		{"allowed_domains set, allow_all_egress unnamed", RestrictionSpec{AllowedDomains: &domains}, false},
		{"allow_all_egress false named", RestrictionSpec{AllowAllEgress: &off}, false},
		{"neither", RestrictionSpec{}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.spec.Overlay().AllowAllEgress
			if got == nil || *got != tc.want {
				t.Errorf("allow_all_egress = %v, want %v", got, tc.want)
			}
		})
	}
}

// A push_rules switch of false narrows nothing and is stored as written, so the false-only rule does not
// refuse it.
func TestRestrictionBoolsFalseOnlyUnchanged(t *testing.T) {
	off := false
	sp := RestrictionSpec{PushRules: &PushRulesOverlay{DenyAll: &off, DenyNewExecutables: &off}}
	if err := RestrictionBoolsFalseOnly(sp, RestrictionLimits{}); err != nil {
		t.Errorf("push_rules.deny_all false refused: %v", err)
	}
	on := true
	if err := RestrictionBoolsFalseOnly(RestrictionSpec{AllowAllEgress: &on}, RestrictionLimits{}); err == nil {
		t.Error("allow_all_egress true accepted")
	}
}
