// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The final seal: the last policy step on every path that dispatches or revives
// a run. Each late host addition (a workspace's or a component's egress, an SCM
// lane, a provider's endpoint) is bounded by its own source and by no outer
// layer, so the seal meets the assembled policy with every outer layer once all
// of them have run. run_policy_seal_census_test.go lists those additions and
// fails on a new one, or on a widening that runs after the seal.

// sealLayer is one outer limit on a run's egress, named as the layer that
// decided a value.
type sealLayer struct {
	name string
	spec types.RunPolicySpec
}

// sealDecision is one knob a layer narrowed.
type sealDecision struct {
	knob, layer string
}

// outerLayers is the ordered outer layers a run is sealed against: the
// organisation walls, then the agent rules. Both are still to come, so a run is
// sealed against none and the seal is the identity.
func (s *Server) outerLayers(context.Context, types.AgentRun) []sealLayer {
	return nil
}

// sealRunPolicy seals policy against the run's outer layers and logs which
// layer decided each knob it narrowed.
func (s *Server) sealRunPolicy(ctx context.Context, run types.AgentRun, policy *types.RunPolicySpec) {
	for _, d := range sealPolicy(policy, s.outerLayers(ctx, run)) {
		slog.InfoContext(ctx, "wardynd: the final seal narrowed the run's policy",
			slog.String("run_id", run.ID.String()), slog.String("knob", d.knob), slog.String("layer", d.layer))
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
		if !slices.Equal(out.AllowedDomains, spec.AllowedDomains) {
			decided = append(decided, sealDecision{"allowed_domains", l.name})
		}
		if out.AllowAllEgress != spec.AllowAllEgress {
			decided = append(decided, sealDecision{"allow_all_egress", l.name})
		}
		if !slices.Equal(out.DeniedDomains, spec.DeniedDomains) {
			decided = append(decided, sealDecision{"denied_domains", l.name})
		}
		spec.AllowedDomains, spec.DeniedDomains, spec.AllowAllEgress = out.AllowedDomains, out.DeniedDomains, out.AllowAllEgress
	}
	return decided
}
