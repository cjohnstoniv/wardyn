// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// restrictionOverlay is a restriction carrying all six narrow-only knobs, as the API applies it.
func restrictionOverlay() Overlay {
	deny := true
	domains := []string{"b.example"}
	rules := []types.ToolRule{{Tool: "Bash", Effect: types.ToolDeny}}
	rec := types.RecordingScreenActions
	models := []string{"model-a"}
	sp := types.RestrictionSpec{
		AllowedDomains: &domains, ToolRules: &rules, PushRules: &types.PushRulesOverlay{DenyAll: &deny},
		Desktop: &types.DesktopControls{CopyOut: types.TransferBlocked, Download: types.TransferBlocked}, Recording: &rec,
	}
	return Overlay{Ceiling: sp.Overlay(), Limits: types.RestrictionLimits{AllowedModels: &models}.Overlay()}
}

// Over a wide authority each knob a restriction carries narrows it, and over a narrow one it never widens it.
func TestApplyOverlayRestrictionKnobsNarrowOnly(t *testing.T) {
	ov := restrictionOverlay()

	wide := Authority{
		Ceiling: types.RunPolicySpec{
			AllowAllEgress: true, AllowedDomains: []string{"a.example", "b.example"}, MinConfinementClass: types.CC1,
			Desktop: &types.DesktopControls{CopyOut: types.TransferAllowed, Download: types.TransferAllowed},
		},
		Limits: types.GovernanceLimits{AllowedModels: []string{"model-a", "model-b"}},
	}
	got := mustApply(t, wide, ov)
	if !Leq(got.Ceiling, wide.Ceiling, got.Limits, wide.Limits) {
		t.Errorf("the result is not at most the authority: %+v", got)
	}
	if got.Ceiling.AllowAllEgress || !slices.Equal(got.Ceiling.AllowedDomains, []string{"b.example"}) {
		t.Errorf("egress = %v %v, want closed with [b.example]", got.Ceiling.AllowAllEgress, got.Ceiling.AllowedDomains)
	}
	if got.Ceiling.PushRules == nil || !got.Ceiling.PushRules.DenyAll {
		t.Errorf("push_rules = %+v, want deny_all", got.Ceiling.PushRules)
	}
	if got.Ceiling.Desktop == nil || got.Ceiling.Desktop.CopyOut != types.TransferBlocked || got.Ceiling.Desktop.Download != types.TransferBlocked {
		t.Errorf("desktop = %+v, want copy_out and download blocked", got.Ceiling.Desktop)
	}
	if got.Ceiling.Recording != types.RecordingScreenActions {
		t.Errorf("recording = %q, want screen_actions", got.Ceiling.Recording)
	}
	if !slices.Equal(got.Limits.AllowedModels, []string{"model-a"}) {
		t.Errorf("allowed_models = %v, want [model-a]", got.Limits.AllowedModels)
	}
	if !slices.ContainsFunc(got.Ceiling.ToolRules, func(r types.ToolRule) bool { return r.Tool == "Bash" && r.Effect == types.ToolDeny }) {
		t.Errorf("tool_rules = %v, want Bash denied", got.Ceiling.ToolRules)
	}

	narrow := Authority{
		Ceiling: types.RunPolicySpec{
			AllowedDomains: []string{"b.example"}, MinConfinementClass: types.CC1, Recording: types.RecordingScreenActions,
			PushRules: &types.PushRulesSpec{DenyAll: true},
			Desktop:   &types.DesktopControls{CopyOut: types.TransferBlocked, Download: types.TransferBlocked},
			ToolRules: []types.ToolRule{{Tool: "Bash", Effect: types.ToolHold}},
		},
		Limits: types.GovernanceLimits{AllowedModels: []string{"model-a"}},
	}
	loose := restrictionOverlay()
	rec, allowed := types.RecordingOff, types.DesktopControls{CopyOut: types.TransferAllowed, Download: types.TransferAllowed}
	loose.Ceiling.Recording, loose.Ceiling.Desktop = &rec, &allowed
	loose.Ceiling.ToolRules = &[]types.ToolRule{{Tool: "Bash", Effect: types.ToolAllow}}
	loose.Ceiling.AllowedDomains = &[]string{"b.example", "c.example"}
	loose.Limits.AllowedModels = &[]string{"model-a", "model-b"}
	off := false
	loose.Ceiling.PushRules = &types.PushRulesOverlay{DenyAll: &off}
	g2, _, err := ApplyOverlay(narrow, loose)
	if err != nil {
		t.Fatalf("ApplyOverlay: %v", err)
	}
	if !Leq(g2.Ceiling, narrow.Ceiling, g2.Limits, narrow.Limits) {
		t.Errorf("a loose restriction widened a narrow authority: %+v", g2)
	}
	if !g2.Ceiling.PushRules.DenyAll || g2.Ceiling.Recording != types.RecordingScreenActions ||
		g2.Ceiling.Desktop.CopyOut != types.TransferBlocked || g2.Ceiling.Desktop.Download != types.TransferBlocked ||
		!slices.Equal(g2.Ceiling.AllowedDomains, []string{"b.example"}) || !slices.Equal(g2.Limits.AllowedModels, []string{"model-a"}) {
		t.Errorf("a loose restriction moved a narrow authority: %+v", g2)
	}
	if !slices.ContainsFunc(g2.Ceiling.ToolRules, func(r types.ToolRule) bool { return r.Tool == "Bash" && r.Effect == types.ToolHold }) ||
		slices.ContainsFunc(g2.Ceiling.ToolRules, func(r types.ToolRule) bool { return r.Tool == "Bash" && r.Effect == types.ToolAllow }) {
		t.Errorf("tool_rules = %v, want the held rule kept", g2.Ceiling.ToolRules)
	}
}

// A restriction's allowlist meets an open run as that allowlist, not as nothing (ApplyRestrictionOverlay):
// open with no list of its own, or with a wider one, the run is closed to the restriction's list; a closed
// run keeps only what both lists name; a restriction naming no allowlist leaves open egress open. An IP
// literal or a port other than 80 and 443 reaches what allow-all does not, so it is kept only where the
// run's own list names it. A unit's
// overlay keeps ApplyOverlay's rule, where open egress covers no entry, so there the open run is closed to
// nothing.
func TestApplyRestrictionOverlayOpenEgressMeetsTheAllowlist(t *testing.T) {
	list := []string{"b.example"}
	closing := Overlay{Ceiling: types.RestrictionSpec{AllowedDomains: &list}.Overlay()}
	denyOnly := Overlay{Ceiling: types.RestrictionSpec{DeniedDomains: &[]string{"c.example"}}.Overlay()}
	exact := []string{"10.0.0.5", "b.example:8443", "c.example:443"}
	exactOnly := Overlay{Ceiling: types.RestrictionSpec{AllowedDomains: &exact}.Overlay()}
	for _, tc := range []struct {
		name string
		base types.RunPolicySpec
		ov   Overlay
		open bool
		want []string
	}{
		{"open with no list", types.RunPolicySpec{AllowAllEgress: true}, closing, false, []string{"b.example"}},
		{"open with a wider list", types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"a.example", "b.example"}}, closing, false, []string{"b.example"}},
		{"closed to another host", types.RunPolicySpec{AllowedDomains: []string{"a.example"}}, closing, false, []string{}},
		{"open under a deny-only restriction", types.RunPolicySpec{AllowAllEgress: true}, denyOnly, true, nil},
		{"open under a literal and another port", types.RunPolicySpec{AllowAllEgress: true}, exactOnly, false, []string{"c.example:443"}},
		{"open naming the literal itself", types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"10.0.0.5"}}, exactOnly, false, []string{"10.0.0.5", "c.example:443"}},
	} {
		got, _, err := ApplyRestrictionOverlay(Authority{Ceiling: tc.base}, tc.ov)
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if got.Ceiling.AllowAllEgress != tc.open || !slices.Equal(got.Ceiling.AllowedDomains, tc.want) {
			t.Errorf("%s: open %v allowed %v, want open %v allowed %v", tc.name, got.Ceiling.AllowAllEgress, got.Ceiling.AllowedDomains, tc.open, tc.want)
		}
	}
	unit := mustApply(t, Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true}}, closing)
	if unit.Ceiling.AllowAllEgress || len(unit.Ceiling.AllowedDomains) != 0 {
		t.Errorf("ApplyOverlay over open egress = open %v allowed %v, want closed to nothing (a unit overlay's rule)", unit.Ceiling.AllowAllEgress, unit.Ceiling.AllowedDomains)
	}
}
