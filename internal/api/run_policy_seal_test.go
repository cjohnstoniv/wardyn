// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"reflect"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func sealFixture() types.RunPolicySpec {
	return types.RunPolicySpec{
		AllowedDomains:   []string{"api.anthropic.com", "github.com", "*.npmjs.org", "evil.example"},
		DeniedDomains:    []string{"Bad.Example"},
		AllowedMethods:   []string{"GET"},
		FirstUseApproval: types.FirstUseWaitForReview,
		WorkspaceMounts:  []types.WorkspaceMount{{Source: "/src", Target: "/workspace"}},
	}
}

// TestSealPolicyWithNoLayersIsTheIdentity: with the outer-layer list empty, as it
// is today, the seal leaves every run's policy exactly as it found it.
func TestSealPolicyWithNoLayersIsTheIdentity(t *testing.T) {
	for _, layers := range [][]sealLayer{nil, {}} {
		got, want := sealFixture(), sealFixture()
		if d := sealPolicy(&got, layers); d != nil || !reflect.DeepEqual(got, want) {
			t.Errorf("sealPolicy(%v) = %v, spec %+v; want no decisions and %+v", layers, d, got, want)
		}
		var zero types.RunPolicySpec
		if sealPolicy(&zero, layers); !reflect.DeepEqual(zero, types.RunPolicySpec{}) {
			t.Errorf("a zero spec became %+v", zero)
		}
	}
	srv := &Server{}
	if l := srv.outerLayers(t.Context(), types.AgentRun{}); len(l) != 0 {
		t.Errorf("outerLayers = %v, want none until the walls land", l)
	}
}

// TestSealPolicyMeetsEachLayer: one layer intersects the allowlist, merges the
// denies and names itself as the deciding layer; nothing else in the spec moves.
func TestSealPolicyMeetsEachLayer(t *testing.T) {
	spec := sealFixture()
	walls := sealLayer{name: "walls", spec: types.RunPolicySpec{
		AllowedDomains: []string{"api.anthropic.com", "github.com", "*.registry.npmjs.org"},
		DeniedDomains:  []string{"gist.github.com"},
	}}
	d := sealPolicy(&spec, []sealLayer{walls})
	if want := []string{"*.registry.npmjs.org", "api.anthropic.com", "github.com"}; !slices.Equal(spec.AllowedDomains, want) {
		t.Errorf("allowed = %v, want %v (evil.example dropped, the wildcard narrowed to the layer's)", spec.AllowedDomains, want)
	}
	if want := []string{"bad.example", "gist.github.com"}; !slices.Equal(spec.DeniedDomains, want) {
		t.Errorf("denied = %v, want %v", spec.DeniedDomains, want)
	}
	if want := []sealDecision{{"allowed_domains", "walls"}, {"denied_domains", "walls"}}; !slices.Equal(d, want) {
		t.Errorf("decisions = %v, want %v", d, want)
	}
	rest, fix := spec, sealFixture()
	rest.AllowedDomains, rest.DeniedDomains = fix.AllowedDomains, fix.DeniedDomains
	if !reflect.DeepEqual(rest, fix) {
		t.Errorf("the seal moved a knob it does not own: %+v", rest)
	}
}

// TestSealPolicyAllowAll: allow_all_egress survives only where every layer
// allows it, and a layer that allows all leaves the allowlist alone.
func TestSealPolicyAllowAll(t *testing.T) {
	spec := types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"github.com"}}
	open := sealLayer{name: "walls", spec: types.RunPolicySpec{AllowAllEgress: true}}
	if d := sealPolicy(&spec, []sealLayer{open}); d != nil || !spec.AllowAllEgress || !slices.Equal(spec.AllowedDomains, []string{"github.com"}) {
		t.Errorf("an allow-all layer changed the spec: %+v, decisions %v", spec, d)
	}
	closed := sealLayer{name: "agent", spec: types.RunPolicySpec{AllowedDomains: []string{"github.com"}}}
	d := sealPolicy(&spec, []sealLayer{open, closed})
	if spec.AllowAllEgress || !slices.Equal(spec.AllowedDomains, []string{"github.com"}) {
		t.Errorf("spec = %+v, want allow-all off and github.com kept", spec)
	}
	if want := []sealDecision{{"allow_all_egress", "agent"}}; !slices.Equal(d, want) {
		t.Errorf("decisions = %v, want %v", d, want)
	}
	nothing := types.RunPolicySpec{AllowedDomains: []string{"github.com"}}
	if sealPolicy(&nothing, []sealLayer{{name: "walls"}}); len(nothing.AllowedDomains) != 0 || nothing.AllowedDomains == nil {
		t.Errorf("a layer that allows nothing left %#v, want the explicit empty allowlist", nothing.AllowedDomains)
	}
}
