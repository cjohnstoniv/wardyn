// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"reflect"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
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

// listLayer is a seal layer that allows only allow (an empty list allows nothing) and denies deny.
func listLayer(name string, allow []string, deny ...string) sealLayer {
	off := false
	l := sealLayer{rec: policyref.Provenance{Layer: name}, egress: types.CeilingOverlay{AllowedDomains: &allow, AllowAllEgress: &off}}
	if len(deny) > 0 {
		l.egress.DeniedDomains = &deny
	}
	return l
}

// TestSealPolicyWithNoLayersIsTheIdentity: with the outer-layer list empty, as it
// is in no_limits, the seal leaves every run's policy exactly as it found it.
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
	walls := listLayer("walls", []string{"api.anthropic.com", "github.com", "*.registry.npmjs.org"}, "gist.github.com")
	d := sealPolicy(&spec, []sealLayer{walls})
	if want := []string{"*.registry.npmjs.org", "api.anthropic.com", "github.com"}; !slices.Equal(spec.AllowedDomains, want) {
		t.Errorf("allowed = %v, want %v (evil.example dropped, the wildcard narrowed to the layer's)", spec.AllowedDomains, want)
	}
	if want := []string{"bad.example", "gist.github.com"}; !slices.Equal(spec.DeniedDomains, want) {
		t.Errorf("denied = %v, want %v", spec.DeniedDomains, want)
	}
	if want := []sealDecision{{"allowed_domains", walls.rec}, {"denied_domains", walls.rec}}; !slices.Equal(d, want) {
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
	on := true
	open := sealLayer{rec: policyref.Provenance{Layer: "walls"}, egress: types.CeilingOverlay{AllowAllEgress: &on}}
	if d := sealPolicy(&spec, []sealLayer{open}); d != nil || !spec.AllowAllEgress || !slices.Equal(spec.AllowedDomains, []string{"github.com"}) {
		t.Errorf("an allow-all layer changed the spec: %+v, decisions %v", spec, d)
	}
	closed := listLayer("agent", []string{"github.com"})
	d := sealPolicy(&spec, []sealLayer{open, closed})
	if spec.AllowAllEgress || !slices.Equal(spec.AllowedDomains, []string{"github.com"}) {
		t.Errorf("spec = %+v, want allow-all off and github.com kept", spec)
	}
	if want := []sealDecision{{"allow_all_egress", closed.rec}}; !slices.Equal(d, want) {
		t.Errorf("decisions = %v, want %v", d, want)
	}
	nothing := types.RunPolicySpec{AllowedDomains: []string{"github.com"}}
	if sealPolicy(&nothing, []sealLayer{listLayer("walls", []string{})}); len(nothing.AllowedDomains) != 0 || nothing.AllowedDomains == nil {
		t.Errorf("a layer that allows nothing left %#v, want the explicit empty allowlist", nothing.AllowedDomains)
	}
}

// sealBoundaryStore is a store holding only a Wardyn boundary, or failing to read it.
type sealBoundaryStore struct {
	store.Store
	b   types.Boundary
	err error
}

func (s sealBoundaryStore) GetBoundary(context.Context) (types.Boundary, error) { return s.b, s.err }

func (s sealBoundaryStore) PutBoundary(context.Context, types.BoundaryWrite, string) (types.Boundary, error) {
	return s.b, nil
}

// sealUnder seals policy for a run with the store st, and returns the provenance it recorded.
func sealUnder(st sealBoundaryStore, policy *types.RunPolicySpec) knobProvenance {
	srv := &Server{cfg: Config{Store: st}}
	prov := knobProvenance{}
	srv.sealRunPolicy(context.Background(), types.AgentRun{ID: uuid.New()}, policy, prov)
	return prov
}

// TestRunPolicySealBindsTheBoundary: in hard_limits the seal re-meets a run's assembled egress with
// the boundary, so a host added after resolve outside its allowlist is cut, its denies are added, a
// late allow-all is turned off, and the boundary is recorded as deciding each knob; a
// deny-only boundary keeps the run's own allowlist. In no_limits the seal changes nothing, and a
// boundary that cannot be read seals the run to no egress.
func TestRunPolicySealBindsTheBoundary(t *testing.T) {
	allowed, denied := []string{"api.anthropic.com", "pypi.org"}, []string{"evil.example"}
	hard := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{AllowedDomains: &allowed, DeniedDomains: &denied}}

	policy := types.RunPolicySpec{AllowedDomains: []string{"api.anthropic.com", "late.example"}}
	prov := sealUnder(sealBoundaryStore{b: hard}, &policy)
	if !slices.Equal(policy.AllowedDomains, []string{"api.anthropic.com"}) || !slices.Contains(policy.DeniedDomains, "evil.example") || policy.AllowAllEgress {
		t.Errorf("sealed = %+v, want only api.anthropic.com allowed and evil.example denied", policy)
	}
	for _, knob := range []string{"allowed_domains", "denied_domains"} {
		if prov[knob].Layer != policyref.LayerBoundary {
			t.Errorf("provenance %s = %+v, want the boundary", knob, prov[knob])
		}
	}

	// Resolve already met an allow-all policy as the boundary's allowlist, so allow-all at the seal is
	// a late widening: it is turned off and only the hosts it lists inside the boundary are kept.
	open := types.RunPolicySpec{AllowAllEgress: true, AllowedDomains: []string{"pypi.org", "late.example"}}
	sealUnder(sealBoundaryStore{b: hard}, &open)
	if open.AllowAllEgress || !slices.Equal(open.AllowedDomains, []string{"pypi.org"}) {
		t.Errorf("allow-all sealed = %+v, want allow-all off and only pypi.org kept", open)
	}

	denyOnly := types.Boundary{Mode: types.BoundaryHardLimits, Spec: types.BoundarySpec{DeniedDomains: &denied}}
	own := types.RunPolicySpec{AllowedDomains: []string{"late.example"}}
	sealUnder(sealBoundaryStore{b: denyOnly}, &own)
	if !slices.Equal(own.AllowedDomains, []string{"late.example"}) || !slices.Contains(own.DeniedDomains, "evil.example") {
		t.Errorf("deny-only sealed = %+v, want its own allowlist kept and evil.example denied", own)
	}

	draft := hard
	draft.Mode = types.BoundaryNoLimits
	got, want := sealFixture(), sealFixture()
	if prov := sealUnder(sealBoundaryStore{b: draft}, &got); len(prov) != 0 || !reflect.DeepEqual(got, want) {
		t.Errorf("no_limits sealed = %+v (%v), want it untouched", got, prov)
	}

	unread := sealFixture()
	sealUnder(sealBoundaryStore{err: errors.New("down")}, &unread)
	if len(unread.AllowedDomains) != 0 || unread.AllowAllEgress {
		t.Errorf("unreadable boundary sealed = %+v, want no egress", unread)
	}
}
