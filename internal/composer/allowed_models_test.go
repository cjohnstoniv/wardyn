// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package composer

import (
	"reflect"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A composed profile narrows allowed_models by intersection, where empty is no limit: an overlay can
// narrow the base's list, never add a model it leaves out, an empty overlay narrows nothing, and a
// disjoint pair is unsatisfiable rather than the empty list, which would be no limit.
func TestAllowedModels_OverlayNarrowsByIntersection(t *testing.T) {
	tests := []struct {
		name string
		base []string
		ov   *[]string
		want []string
	}{
		{"an overlay narrows an unlimited base", nil, &[]string{"model-a"}, []string{"model-a"}},
		{"an overlay narrows the base's list", []string{"model-a", "small-model-a"}, &[]string{"model-a"}, []string{"model-a"}},
		{"an empty overlay list keeps the base's", []string{"model-a"}, &[]string{}, []string{"model-a"}},
		{"an empty overlay over an unlimited base stays unlimited", nil, &[]string{}, nil},
		{"an overlay that names nothing keeps the base's", []string{"model-a"}, nil, []string{"model-a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mustApply(t, Authority{Limits: types.GovernanceLimits{AllowedModels: tc.base}},
				limitsOverlay(types.LimitsOverlay{AllowedModels: tc.ov}))
			if !reflect.DeepEqual(got.Limits.AllowedModels, tc.want) {
				t.Errorf("allowed_models = %v, want %v", got.Limits.AllowedModels, tc.want)
			}
		})
	}
	t.Run("a model the base excludes is dropped with a warning", func(t *testing.T) {
		base := Authority{Limits: types.GovernanceLimits{AllowedModels: []string{"model-a", "small-model-a"}}}
		got, warnings, err := ApplyOverlay(base, limitsOverlay(types.LimitsOverlay{AllowedModels: &[]string{"model-a", "model-b"}}))
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(got.Limits.AllowedModels, []string{"model-a"}) {
			t.Errorf("allowed_models = %v, want [model-a]", got.Limits.AllowedModels)
		}
		if !strings.Contains(strings.Join(warnings, "\n"), "allowed_models: the base excludes model-b") {
			t.Errorf("warnings = %v, want the base excludes model-b", warnings)
		}
	})
	t.Run("a disjoint overlay is unsatisfiable, not no limit", func(t *testing.T) {
		base := Authority{Limits: types.GovernanceLimits{AllowedModels: []string{"model-a", "small-model-a"}}}
		_, _, err := ApplyOverlay(base, limitsOverlay(types.LimitsOverlay{AllowedModels: &[]string{"model-b"}}))
		wantOverlayErr(t, err, ReasonOverlayUnsatisfiable, "allowed_models")
		if err == nil || !strings.Contains(err.Error(), "no model is in both the base (model-a,small-model-a) and the overlay (model-b)") {
			t.Errorf("err = %v, want the disjoint-list sentence", err)
		}
	})
	t.Run("an overlay listing a model twice is invalid", func(t *testing.T) {
		_, _, err := ApplyOverlay(Authority{}, limitsOverlay(types.LimitsOverlay{AllowedModels: &[]string{"model-a", "model-a"}}))
		wantOverlayErr(t, err, ReasonOverlayInvalid, "allowed_models")
	})
}

func TestAllowedModels_Leq(t *testing.T) {
	lim := func(m ...string) types.GovernanceLimits { return types.GovernanceLimits{AllowedModels: m} }
	for _, tc := range []struct {
		a, b types.GovernanceLimits
		want bool
	}{
		{lim("model-a"), lim(), true},
		{lim(), lim(), true},
		{lim("model-a"), lim("model-a", "small-model-a"), true},
		{lim(), lim("model-a"), false},
		{lim("model-b"), lim("model-a"), false},
	} {
		if got := leqLimits(tc.a, tc.b); got != tc.want {
			t.Errorf("leqLimits(%v, %v) = %v, want %v", tc.a.AllowedModels, tc.b.AllowedModels, got, tc.want)
		}
	}
}
