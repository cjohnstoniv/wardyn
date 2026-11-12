// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	denyA = "11111111-1111-4111-8111-111111111111"
	denyB = "22222222-2222-4222-8222-222222222222"
	denyC = "33333333-3333-4333-8333-333333333333"
)

// denyLimits puts ids into the limits field a case names.
func denyLimits(field string, ids ...string) types.GovernanceLimits {
	if field == "deny_components" {
		return types.GovernanceLimits{DenyComponents: ids}
	}
	return types.GovernanceLimits{DenyModelProviders: ids}
}

func denyOverlay(field string, ids *[]string) types.LimitsOverlay {
	if field == "deny_components" {
		return types.LimitsOverlay{DenyComponents: ids}
	}
	return types.LimitsOverlay{DenyModelProviders: ids}
}

func denyOf(field string, l types.GovernanceLimits) []string {
	if field == "deny_components" {
		return l.DenyComponents
	}
	return l.DenyModelProviders
}

// TestMeetDenyListsUnion: a deny list only ever grows. An overlay's ids join the base's (base order
// first, each once); an empty or silent overlay keeps the base's list. Deny wins.
func TestMeetDenyListsUnion(t *testing.T) {
	ids := func(s ...string) *[]string { return &s }
	for _, field := range []string{"deny_components", "deny_model_providers"} {
		for _, tc := range []struct {
			name string
			base []string
			ov   *[]string
			want []string
		}{
			{"an overlay adds to an empty base", nil, ids(denyA), []string{denyA}},
			{"the overlay's ids join the base's, base first", []string{denyB}, ids(denyA), []string{denyB, denyA}},
			{"the same id twice is one", []string{denyA}, ids(denyA, denyB), []string{denyA, denyB}},
			{"an empty overlay list does not lift the base's", []string{denyA}, ids(), []string{denyA}},
			{"a silent overlay keeps the base's", []string{denyA}, nil, []string{denyA}},
			{"nothing on either side is nothing", nil, nil, nil},
		} {
			t.Run(field+"/"+tc.name, func(t *testing.T) {
				got := mustApply(t, Authority{Limits: denyLimits(field, tc.base...)}, limitsOverlay(denyOverlay(field, tc.ov)))
				if g := denyOf(field, got.Limits); !reflect.DeepEqual(g, tc.want) {
					t.Errorf("%s = %v, want %v", field, g, tc.want)
				}
			})
		}
		t.Run(field+"/an id that is not an id is invalid", func(t *testing.T) {
			_, _, err := ApplyOverlay(Authority{}, limitsOverlay(denyOverlay(field, ids("corp-gateway"))))
			wantOverlayErr(t, err, ReasonOverlayInvalid, field)
		})
	}
}

// TestLeqDenyListsSuperset: a result that drops an id the base denies is wider; one that adds an id,
// or keeps the list, is no wider.
func TestLeqDenyListsSuperset(t *testing.T) {
	var spec types.RunPolicySpec
	for _, field := range []string{"deny_components", "deny_model_providers"} {
		for _, tc := range []struct {
			name string
			a, b []string
			want bool
		}{
			{"dropped entry", []string{denyA}, []string{denyA, denyB}, false},
			{"emptied list", nil, []string{denyA}, false},
			{"swapped entry", []string{denyB}, []string{denyA}, false},
			{"same list", []string{denyA, denyB}, []string{denyA, denyB}, true},
			{"same entries, other order", []string{denyB, denyA}, []string{denyA, denyB}, true},
			{"added entry", []string{denyA, denyB}, []string{denyA}, true},
			{"first entry", []string{denyA}, nil, true},
			{"both empty", nil, nil, true},
		} {
			if got := Leq(spec, spec, denyLimits(field, tc.a...), denyLimits(field, tc.b...)); got != tc.want {
				t.Errorf("%s/%s: Leq(%v, %v) = %v, want %v", field, tc.name, tc.a, tc.b, got, tc.want)
			}
		}
	}
}

// TestClampDenyListsAreNotASpecBound: the deny lists are a door, not a bound on the run's policy, so
// Clamp's answer is the same with them set as with them empty, and the lists survive in the limits
// it was given.
func TestClampDenyListsAreNotASpecBound(t *testing.T) {
	proposed := types.RunPolicySpec{AllowedDomains: []string{"github.com"}, MinConfinementClass: types.CC2}
	ceiling := types.RunPolicySpec{AllowedDomains: []string{"github.com", "pypi.org"}, MinConfinementClass: types.CC2}
	limits := types.GovernanceLimits{DenyComponents: []string{denyA}, DenyModelProviders: []string{denyB}}
	want, wantWarns := Clamp(proposed, ceiling, types.GovernanceLimits{})
	got, gotWarns := Clamp(proposed, ceiling, limits)
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(gotWarns, wantWarns) {
		t.Errorf("Clamp changed with deny lists set: %+v %v, want %+v %v", got, gotWarns, want, wantWarns)
	}
	if !reflect.DeepEqual(limits.DenyComponents, []string{denyA}) || !reflect.DeepEqual(limits.DenyModelProviders, []string{denyB}) {
		t.Errorf("Clamp altered the limits it was given: %+v", limits)
	}
}
