// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestPersonalWallsSpecOverlay(t *testing.T) {
	list := []string{"pypi.org"}
	off, on := false, true
	t.Run("an allowlist turns open egress off", func(t *testing.T) {
		o := PersonalWallsSpec{AllowedDomains: &list}.Overlay()
		if o.AllowAllEgress == nil || *o.AllowAllEgress {
			t.Fatalf("allow_all_egress = %v, want false", o.AllowAllEgress)
		}
	})
	t.Run("a deny list leaves open egress alone", func(t *testing.T) {
		if o := (PersonalWallsSpec{DeniedDomains: &list}).Overlay(); o.AllowAllEgress != nil {
			t.Fatalf("allow_all_egress = %v, want unset", *o.AllowAllEgress)
		}
	})
	t.Run("a named allow_all_egress is kept", func(t *testing.T) {
		if o := (PersonalWallsSpec{AllowedDomains: &list, AllowAllEgress: &on}).Overlay(); o.AllowAllEgress == nil || !*o.AllowAllEgress {
			t.Fatal("a named value was overwritten")
		}
		if o := (PersonalWallsSpec{AllowAllEgress: &off}).Overlay(); o.AllowAllEgress == nil || *o.AllowAllEgress {
			t.Fatal("false was lost")
		}
	})
	t.Run("an unnamed desktop direction is allowed, not its default", func(t *testing.T) {
		d := PersonalWallsSpec{Desktop: &DesktopControls{PasteIn: TransferBlocked}}.Overlay().Desktop
		if d == nil || *d != (DesktopControls{PasteIn: TransferBlocked, CopyOut: TransferAllowed, Upload: TransferAllowed, Download: TransferAllowed}) {
			t.Fatalf("desktop = %+v", d)
		}
	})
	t.Run("no desktop stays no desktop", func(t *testing.T) {
		if o := (PersonalWallsSpec{}).Overlay(); o.Desktop != nil {
			t.Fatalf("desktop = %+v", o.Desktop)
		}
	})
}

func TestPersonalWallsSpecEmpty(t *testing.T) {
	none := []string{}
	cc := CC2
	for name, tc := range map[string]struct {
		s    PersonalWallsSpec
		want bool
	}{
		"nothing":               {PersonalWallsSpec{}, true},
		"a desktop naming none": {PersonalWallsSpec{Desktop: &DesktopControls{}}, true},
		"an empty allowlist":    {PersonalWallsSpec{AllowedDomains: &none}, false},
		"a floor":               {PersonalWallsSpec{MinConfinementClass: &cc}, false},
		"a blocked direction":   {PersonalWallsSpec{Desktop: &DesktopControls{Upload: TransferBlocked}}, false},
		"a withheld grant":      {PersonalWallsSpec{WithheldGrants: []GrantSpec{{Kind: GrantAPIKey}}}, false},
	} {
		if got := tc.s.Empty(); got != tc.want {
			t.Errorf("%s: Empty() = %v, want %v", name, got, tc.want)
		}
	}
}
