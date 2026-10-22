// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The final seal: the last policy step on every path that dispatches or revives
// a run. Each late host addition (a workspace's or a component's egress, an SCM
// lane, a provider's endpoint) is bounded by its own source and by no outer
// layer, so the seal meets the assembled policy with every outer layer once all
// of them have run. run_policy_seal_census_test.go lists those additions and
// fails on a new one, or on a widening that runs after the seal.

// sealLayer is one outer limit on a run's egress, and the record a knob it
// decides carries in the run's provenance. egress holds only the three egress
// knobs, each nil when the layer does not bound it: AllowedDomains is an
// allowlist (an empty one allows nothing), AllowAllEgress false turns open
// egress off and keeps the run's own allowlist, and DeniedDomains is added. A
// layer that only denies leaves the first two nil. grants is the credential
// grants the layer lets a run be given, nil for no limit (an empty list allows
// none); the seal never reads it, the withholding after it does.
type sealLayer struct {
	rec    policyref.Provenance
	egress types.CeilingOverlay
	grants *[]types.GrantSpec
}

// sealDecision is one knob a layer narrowed.
type sealDecision struct {
	knob  string
	layer policyref.Provenance
}

// outerLayers is the ordered outer layers a run is sealed against, outermost
// first: the Wardyn boundary, then the owner's restrictions (still to come).
// Unit walls are inner layers, applied at resolve, never here. In no_limits there
// is no layer, so the seal is the identity. A boundary that cannot be read seals
// the run to no egress and no credential at all: an unread boundary is never no
// boundary.
func (s *Server) outerLayers(ctx context.Context, run types.AgentRun) []sealLayer {
	b, err := s.governanceBoundary(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: read the Wardyn boundary at the seal; the run is sealed to no egress",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
		off := false
		return []sealLayer{{rec: boundaryProvenance, egress: types.CeilingOverlay{AllowedDomains: &[]string{}, AllowAllEgress: &off},
			grants: &[]types.GrantSpec{}}}
	}
	if !b.HardLimits() {
		return nil
	}
	return []sealLayer{{rec: boundaryProvenance, egress: boundarySealEgress(b.Spec), grants: b.Spec.EligibleGrants}}
}

// boundarySealEgress is the boundary's egress as a seal layer: its allowlist when it names one and
// does not also allow all egress; allow_all_egress as named, or false beside an allowlist, as the
// resolve meet reads it (types.BoundarySpec.Overlay); its denies either way. An explicit false with
// no allowlist turns open egress off and keeps the run's own allowlist, so a run that was open when
// it was frozen is closed at its revive. Naming neither leaves a deny-only layer.
func boundarySealEgress(sp types.BoundarySpec) types.CeilingOverlay {
	var out types.CeilingOverlay
	open := sp.AllowAllEgress != nil && *sp.AllowAllEgress
	if sp.AllowedDomains != nil && !open {
		list := slices.Clone(*sp.AllowedDomains)
		if list == nil {
			list = []string{}
		}
		out.AllowedDomains = &list
	}
	switch {
	case sp.AllowAllEgress != nil:
		v := *sp.AllowAllEgress
		out.AllowAllEgress = &v
	case out.AllowedDomains != nil:
		off := false
		out.AllowAllEgress = &off
	}
	if sp.DeniedDomains != nil && len(*sp.DeniedDomains) > 0 {
		deny := slices.Clone(*sp.DeniedDomains)
		out.DeniedDomains = &deny
	}
	return out
}

// sealRunPolicy seals policy against the run's outer layers and records which
// layer decided each knob it narrowed: in prov when the path keeps a provenance
// record (dispatch; nil on a revive), and in the log. Where several layers narrow
// one knob, the last of them, the innermost, is the one recorded. It returns the
// layers it read, so the credentials the path withholds next are judged against
// the same read (withholding).
func (s *Server) sealRunPolicy(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, prov knobProvenance) []sealLayer {
	layers := s.outerLayers(ctx, run)
	for _, d := range sealPolicy(policy, layers) {
		if prov != nil {
			prov[d.knob] = d.layer
		}
		slog.InfoContext(ctx, "wardynd: the final seal narrowed the run's policy",
			slog.String("run_id", run.ID.String()), slog.String("knob", d.knob), slog.String("layer", d.layer.Layer))
	}
	return layers
}

// sealPolicy meets spec with each layer in order, by the governance meet
// (composer.ApplyOverlay): the allowlist is intersected, allow_all_egress
// survives only where every layer allows it, and the deny lists are merged.
// Only those three knobs are read, so nothing else in spec can change. With no
// layers spec is untouched.
func sealPolicy(spec *types.RunPolicySpec, layers []sealLayer) []sealDecision {
	var decided []sealDecision
	for _, l := range layers {
		in := types.RunPolicySpec{AllowedDomains: spec.AllowedDomains, DeniedDomains: spec.DeniedDomains, AllowAllEgress: spec.AllowAllEgress}
		met, _, err := composer.ApplyOverlay(composer.Authority{Ceiling: in}, composer.Overlay{Ceiling: l.egress})
		if err != nil {
			// The egress knobs alone have no unsatisfiable meet; should one appear, nothing is reachable.
			met.Ceiling = types.RunPolicySpec{AllowedDomains: []string{}, DeniedDomains: spec.DeniedDomains}
		}
		out := met.Ceiling
		// Compared canonical, as the provenance record compares, so a re-spelling decides nothing.
		before, after := canonicalKnobs(in), canonicalKnobs(out)
		if !slices.Equal(before.AllowedDomains, after.AllowedDomains) {
			decided = append(decided, sealDecision{"allowed_domains", l.rec})
		}
		if before.AllowAllEgress != after.AllowAllEgress {
			decided = append(decided, sealDecision{"allow_all_egress", l.rec})
		}
		if !slices.Equal(before.DeniedDomains, after.DeniedDomains) {
			decided = append(decided, sealDecision{"denied_domains", l.rec})
		}
		spec.AllowedDomains, spec.DeniedDomains, spec.AllowAllEgress = out.AllowedDomains, out.DeniedDomains, out.AllowAllEgress
	}
	return decided
}
