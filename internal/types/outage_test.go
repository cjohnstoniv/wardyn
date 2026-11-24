// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "testing"

func TestEffectiveOutageTerms_PolicyOnlyShortens(t *testing.T) {
	for _, tc := range []struct {
		name   string
		site   int
		policy *OutageControls
		want   OutageTerms
	}{
		{"default", 0, nil, OutageTerms{AllowanceSec: 3600}},
		{"policy cannot lengthen", 120, &OutageControls{AllowanceMinutes: 240}, OutageTerms{AllowanceSec: 7200}},
		{"policy shortens", 120, &OutageControls{AllowanceMinutes: 30}, OutageTerms{AllowanceSec: 1800}},
		{"site maximum, policy unset", 1440, &OutageControls{}, OutageTerms{AllowanceSec: 86400}},
		{"strict", 0, &OutageControls{Strict: true}, OutageTerms{AllowanceSec: 3600, Strict: true}},
		{"end unattended", 0, &OutageControls{Unattended: OutageUnattendedEnd}, OutageTerms{AllowanceSec: 3600, EndUnattended: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := EffectiveOutageTerms(tc.site, tc.policy); got != tc.want {
				t.Fatalf("EffectiveOutageTerms(%d, %+v) = %+v, want %+v", tc.site, tc.policy, got, tc.want)
			}
		})
	}
}

func TestOutageControlsMeet(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b *OutageControls
		want OutageControls
	}{
		{"smaller positive allowance", &OutageControls{AllowanceMinutes: 120}, &OutageControls{AllowanceMinutes: 30}, OutageControls{AllowanceMinutes: 30}},
		{"zero is the identity", &OutageControls{}, &OutageControls{AllowanceMinutes: 90}, OutageControls{AllowanceMinutes: 90}},
		{"nil is the identity", nil, &OutageControls{AllowanceMinutes: 90}, OutageControls{AllowanceMinutes: 90}},
		{"strict ORs", &OutageControls{Strict: true}, &OutageControls{}, OutageControls{Strict: true}},
		{"end wins over keep", &OutageControls{Unattended: OutageUnattendedKeep}, &OutageControls{Unattended: OutageUnattendedEnd}, OutageControls{Unattended: OutageUnattendedEnd}},
		{"end wins over unset", &OutageControls{Unattended: OutageUnattendedEnd}, &OutageControls{}, OutageControls{Unattended: OutageUnattendedEnd}},
		{"keep over unset", &OutageControls{}, &OutageControls{Unattended: OutageUnattendedKeep}, OutageControls{Unattended: OutageUnattendedKeep}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Meet(tc.b); got != tc.want {
				t.Fatalf("%+v meet %+v = %+v, want %+v", tc.a, tc.b, got, tc.want)
			}
			if got := tc.b.Meet(tc.a); got != tc.want {
				t.Fatalf("%+v meet %+v = %+v, want %+v (the meet is symmetric)", tc.b, tc.a, got, tc.want)
			}
		})
	}
}

func TestValidateOutageControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		o    *OutageControls
		ok   bool
	}{
		{"nil", nil, true},
		{"zero", &OutageControls{}, true},
		{"30", &OutageControls{AllowanceMinutes: 30}, true},
		{"1440", &OutageControls{AllowanceMinutes: 1440, Unattended: OutageUnattendedKeep}, true},
		{"end", &OutageControls{Unattended: OutageUnattendedEnd}, true},
		{"29", &OutageControls{AllowanceMinutes: 29}, false},
		{"1441", &OutageControls{AllowanceMinutes: 1441}, false},
		{"unattended x", &OutageControls{Unattended: "x"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateOutageControls(tc.o); (err == nil) != tc.ok {
				t.Fatalf("ValidateOutageControls(%+v) = %v, want ok %v", tc.o, err, tc.ok)
			}
		})
	}
}

func TestOutageTermsIsDefault(t *testing.T) {
	if !DefaultOutageTerms().IsDefault() || !EffectiveOutageTerms(0, nil).IsDefault() {
		t.Fatal("the default terms are not default")
	}
	for _, t2 := range []OutageTerms{{AllowanceSec: 1800}, {AllowanceSec: 3600, Strict: true}, {AllowanceSec: 3600, EndUnattended: true}} {
		if t2.IsDefault() {
			t.Fatalf("%+v reads as default", t2)
		}
	}
}
