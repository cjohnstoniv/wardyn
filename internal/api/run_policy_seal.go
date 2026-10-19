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
// decides carries in the run's provenance.
type sealLayer struct {
	rec  policyref.Provenance
	spec types.RunPolicySpec
}

// sealDecision is one knob a layer narrowed.
type sealDecision struct {
	knob  string
	layer policyref.Provenance
}

// outerLayers is the ordered outer layers a run is sealed against: the
// organisation walls, then the agent rules. Both are still to come, so a run is
// sealed against none and the seal is the identity.
func (s *Server) outerLayers(context.Context, types.AgentRun) []sealLayer {
	return nil
}

// sealRunPolicy seals policy against the run's outer layers and records which
// layer decided each knob it narrowed: in prov when the path keeps a provenance
// record (dispatch; nil on a revive), and in the log.
func (s *Server) sealRunPolicy(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec, prov knobProvenance) {
	for _, d := range sealPolicy(policy, s.outerLayers(ctx, run)) {
		if prov != nil {
			prov[d.knob] = d.layer
		}
		slog.InfoContext(ctx, "wardynd: the final seal narrowed the run's policy",
			slog.String("run_id", run.ID.String()), slog.String("knob", d.knob), slog.String("layer", d.layer.Layer))
	}
}

// sealPolicy meets spec with each layer in order, by the governance meet
// (composer.ApplyOverlay): the allowlist is intersected, allow_all_egress
// survives only where every layer allows it, and the deny lists are merged.
// Only those three knobs are read, so nothing else in spec can change. With no
// layers spec is untouched.
func sealPolicy(spec *types.RunPolicySpec, layers []sealLayer) []sealDecision {
	var decided []sealDecision
	for _, l := range layers {
		ov := types.CeilingOverlay{AllowAllEgress: &l.spec.AllowAllEgress}
		if !l.spec.AllowAllEgress {
			ov.AllowedDomains = &l.spec.AllowedDomains
		}
		if len(l.spec.DeniedDomains) > 0 {
			ov.DeniedDomains = &l.spec.DeniedDomains
		}
		in := types.RunPolicySpec{AllowedDomains: spec.AllowedDomains, DeniedDomains: spec.DeniedDomains, AllowAllEgress: spec.AllowAllEgress}
		met, _, err := composer.ApplyOverlay(composer.Authority{Ceiling: in}, composer.Overlay{Ceiling: ov})
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
