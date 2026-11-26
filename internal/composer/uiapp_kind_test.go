// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestClamp_UIAppsDesktopKindFromCeiling(t *testing.T) {
	clampApps := func(proposal, ceiling []types.UIApp) ([]types.UIApp, []string) {
		c := operatorCeiling(t)
		c.UIApps = ceiling
		out, warns := Clamp(types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: proposal}, c, types.GovernanceLimits{})
		return out.UIApps, warns
	}
	desk, deskDesktop := types.UIApp{Name: "desk", Port: 5900}, types.UIApp{Name: "desk", Port: 5900, Kind: types.UIAppKindDesktop}

	apps, warns := clampApps([]types.UIApp{desk}, []types.UIApp{deskDesktop})
	if len(apps) != 1 || apps[0].Kind != types.UIAppKindDesktop {
		t.Errorf("a ceiling desktop entry: kept %+v, want the app marked desktop", apps)
	}
	if !hasWarn(warns, "marked 1 ui_app(s) desktop") {
		t.Errorf("warnings = %v, want the marked line", warns)
	}

	apps, warns = clampApps([]types.UIApp{deskDesktop}, []types.UIApp{deskDesktop})
	if len(apps) != 1 || apps[0].Kind != types.UIAppKindDesktop || hasWarn(warns, "marked") {
		t.Errorf("a proposal already desktop: kept %+v, warnings %v, want it unchanged and no marked line", apps, warns)
	}

	code, codeDesktop := types.UIApp{Name: "code", Port: 8080}, types.UIApp{Name: "code", Port: 8080, Kind: types.UIAppKindDesktop}
	apps, _ = clampApps([]types.UIApp{codeDesktop}, []types.UIApp{code})
	if len(apps) != 1 || apps[0].Kind != types.UIAppKindDesktop {
		t.Errorf("a proposal may narrow: kept %+v, want the proposal's kind", apps)
	}

	novnc := types.UIApp{Name: "novnc", Port: 6080}
	apps, warns = clampApps([]types.UIApp{novnc}, []types.UIApp{novnc})
	if len(apps) != 1 || apps[0].Kind != "" || hasWarn(warns, "marked") {
		t.Errorf("novnc: kept %+v, warnings %v, want Kind empty (a desktop by name) and no warning", apps, warns)
	}
}

func TestLeqUIApps_DesktopKind(t *testing.T) {
	desk, deskDesktop := types.UIApp{Name: "desk", Port: 5900}, types.UIApp{Name: "desk", Port: 5900, Kind: types.UIAppKindDesktop}
	if leqUIApps([]types.UIApp{desk}, []types.UIApp{deskDesktop}) {
		t.Error("an app without the kind is within b's desktop entry")
	}
	if !leqUIApps([]types.UIApp{deskDesktop}, []types.UIApp{desk}) {
		t.Error("a desktop app is not within b's plain entry")
	}
	novnc, novncDesktop := types.UIApp{Name: "novnc", Port: 6080}, types.UIApp{Name: "novnc", Port: 6080, Kind: types.UIAppKindDesktop}
	if !leqUIApps([]types.UIApp{novnc}, []types.UIApp{novncDesktop}) {
		t.Error("novnc is a desktop by name, so it is within b's novnc desktop entry")
	}
	a := types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{desk}}
	b := types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{deskDesktop}}
	if Leq(a, b, types.GovernanceLimits{}, types.GovernanceLimits{}) {
		t.Error("Leq: a spec that drops b's desktop kind is within b")
	}
	if !Leq(b, a, types.GovernanceLimits{}, types.GovernanceLimits{}) {
		t.Error("Leq: a spec that adds the kind is not within b")
	}
}

func TestMeetUIApps_DesktopKind(t *testing.T) {
	type result struct {
		base, got     Authority
		warns         []string
		verr, applyEr error
	}
	apply := func(base, ov types.UIApp) result {
		b := Authority{Ceiling: types.RunPolicySpec{MinConfinementClass: types.CC2, UIApps: []types.UIApp{base}}}
		o := overlayOf(types.CeilingOverlay{UIApps: &[]types.UIApp{ov}})
		verr := ValidateOverlay(b, o)
		got, warns, aerr := ApplyOverlay(b, o)
		return result{b, got, warns, verr, aerr}
	}
	desk := types.UIApp{Name: "desk", Port: 8000}
	deskDesktop := types.UIApp{Name: "desk", Port: 8000, Kind: types.UIAppKindDesktop}

	r := apply(desk, deskDesktop)
	if r.verr != nil || r.applyEr != nil {
		t.Fatalf("overlay adds the kind: ValidateOverlay %v, ApplyOverlay %v", r.verr, r.applyEr)
	}
	if r.got.Ceiling.UIApps[0].Kind != types.UIAppKindDesktop {
		t.Errorf("kept %+v, want the overlay's desktop kind", r.got.Ceiling.UIApps)
	}
	if !Leq(r.got.Ceiling, r.base.Ceiling, r.got.Limits, r.base.Limits) {
		t.Errorf("Leq(result, base) is false: %+v", r.got.Ceiling.UIApps)
	}

	r = apply(deskDesktop, desk)
	if r.verr != nil || r.applyEr != nil || r.got.Ceiling.UIApps[0].Kind != types.UIAppKindDesktop {
		t.Errorf("base desktop, overlay plain: ValidateOverlay %v, ApplyOverlay %v, kept %+v, want the base's kind", r.verr, r.applyEr, r.got.Ceiling.UIApps)
	}

	r = apply(desk, types.UIApp{Name: "desk", Port: 8000, Kind: "Desktop"})
	wantOverlayErr(t, r.verr, ReasonOverlayInvalid, "ui_apps")
	if r.applyEr != nil || len(r.got.Ceiling.UIApps) != 1 || r.got.Ceiling.UIApps[0].Kind != "" {
		t.Errorf("a misspelt kind: ApplyOverlay %v, kept %+v, want the app with Kind empty", r.applyEr, r.got.Ceiling.UIApps)
	}
	if !hasWarn(r.warns, "invalid kind") {
		t.Errorf("warnings = %v, want one containing %q", r.warns, "invalid kind")
	}
}
