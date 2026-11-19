// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"context"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/egress/proxy"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// unitClosing is a unit overlay that turns open egress off with list.
func unitClosing(list ...string) Overlay {
	return Overlay{Ceiling: types.CeilingOverlay{AllowAllEgress: new(false), AllowedDomains: &list}}
}

// A unit that locks itself to one internal host under a base open to all egress with its own list is a
// narrowing: the save accepts it and the resolve closes the unit to exactly that host.
func TestUnitOverlayClosesAnOpenBaseToItsList(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"github.com", "pypi.org", "*.npmjs.org"}}}
	ov := unitClosing("build.corp.example")
	if err := ValidateOverlay(base, ov); err == nil {
		t.Fatal("ValidateOverlay accepted the entry; the plain meet should still refuse it")
	}
	if err := ValidateUnitOverlay(base, ov); err != nil {
		t.Fatalf("ValidateUnitOverlay: %v", err)
	}
	got, warns, err := ApplyUnitOverlay(base, ov)
	if err != nil {
		t.Fatal(err)
	}
	if got.Ceiling.AllowAllEgress || !slices.Equal(got.Ceiling.AllowedDomains, []string{"build.corp.example"}) || len(warns) != 0 {
		t.Fatalf("open %v allowed %v warns %v, want closed to build.corp.example", got.Ceiling.AllowAllEgress, got.Ceiling.AllowedDomains, warns)
	}
	if !slices.Equal(base.Ceiling.AllowedDomains, []string{"github.com", "pypi.org", "*.npmjs.org"}) {
		t.Fatalf("base list changed to %v", base.Ceiling.AllowedDomains)
	}
}

// Open egress reaches a name on 80 and 443 only and trusts no IP literal, so a unit entry outside those is
// still measured against the base's own list: refused at save, dropped at resolve.
func TestUnitOverlayOpenBaseStillRefusesLiteralsAndOtherPorts(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"github.com"}}}
	for _, entry := range []string{"10.0.0.5", "build.corp.example:8443"} {
		ov := unitClosing("build.corp.example", entry)
		wantOverlayErr(t, ValidateUnitOverlay(base, ov), ReasonOverlayInvalid, "allowed_domains")
		got, warns, err := ApplyUnitOverlay(base, ov)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got.Ceiling.AllowedDomains, []string{"build.corp.example"}) || len(warns) != 1 {
			t.Errorf("%s: allowed %v warns %v, want only build.corp.example and one warning", entry, got.Ceiling.AllowedDomains, warns)
		}
	}
	named := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"10.0.0.5", "build.corp.example:8443"}}}
	if err := ValidateUnitOverlay(named, unitClosing("10.0.0.5", "build.corp.example:8443")); err != nil {
		t.Errorf("entries the base names itself: %v", err)
	}
}

// The base's denied_domains join the unit's result, so a host the base denies stays unreachable however the
// unit's list names it.
func TestUnitOverlayOpenBaseKeepsTheBaseDenies(t *testing.T) {
	base := Authority{Ceiling: types.RunPolicySpec{AllowAllEgress: true, DeniedDomains: []string{"*.corp.example"}}}
	got, _, err := ApplyUnitOverlay(base, unitClosing("build.corp.example", "pypi.org"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got.Ceiling.DeniedDomains, "*.corp.example") {
		t.Fatalf("denied %v, want the base's deny kept", got.Ceiling.DeniedDomains)
	}
	ev := proxy.NewBuiltinEvaluator(got.Ceiling)
	for host, want := range map[string]egress.HostVerdict{"build.corp.example": egress.VerdictDeny, "pypi.org": egress.VerdictAllow} {
		if v, _ := ev.EvaluateHost(context.Background(), egress.Request{Host: host, Port: 443}); (v == egress.VerdictAllow) != (want == egress.VerdictAllow) {
			t.Errorf("%s: verdict %v, want %v", host, v, want)
		}
	}
}

// A base that is not open to all egress, or an overlay that leaves open egress on, is read as before.
func TestUnitOverlayOtherwiseIsThePlainMeet(t *testing.T) {
	list := []string{"build.corp.example"}
	for _, tc := range []struct {
		name string
		base types.RunPolicySpec
		ov   Overlay
	}{
		{"closed base", types.RunPolicySpec{AllowedDomains: []string{"github.com"}}, unitClosing(list...)},
		{"open base, overlay keeps it open", types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"github.com"}}, Overlay{Ceiling: types.CeilingOverlay{AllowedDomains: &list}}},
	} {
		base := Authority{Ceiling: tc.base}
		wantOverlayErr(t, ValidateUnitOverlay(base, tc.ov), ReasonOverlayInvalid, "allowed_domains")
		got, warns, err := ApplyUnitOverlay(base, tc.ov)
		plain, plainWarns, plainErr := ApplyOverlay(base, tc.ov)
		if err != nil || plainErr != nil {
			t.Fatalf("%s: %v / %v", tc.name, err, plainErr)
		}
		if got.Ceiling.AllowAllEgress != plain.Ceiling.AllowAllEgress || !slices.Equal(got.Ceiling.AllowedDomains, plain.Ceiling.AllowedDomains) || !slices.Equal(warns, plainWarns) {
			t.Errorf("%s: got open %v allowed %v, plain meet open %v allowed %v", tc.name, got.Ceiling.AllowAllEgress, got.Ceiling.AllowedDomains, plain.Ceiling.AllowAllEgress, plain.Ceiling.AllowedDomains)
		}
	}
}
