// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// A run's CPU and memory are bounded after placement by the strictest of the pool, the person's
// governance, the deployment and (on their own runner) the runner's capacity: an over-ask is reduced,
// never refused. Three inputs stay apart (DR-0112): the ASK is the request's size, else its source
// policy's own; the BASELINE governance bounds a member from is the admin-authored resolved size; the
// spec the run executes is the applied size the fold writes back.

// resourceAsk is what a request asks for, per field: its own value, else its source policy's
// authored one (sourceSize), else 0, which asks for the default.
func resourceAsk(req *client.RunResources, own types.ResourceLimits) types.ResourceLimits {
	out := own
	if req != nil {
		out.CPUMillis, out.MemoryMiB = cmp.Or(req.CPUMillis, out.CPUMillis), cmp.Or(req.MemoryMiB, out.MemoryMiB)
	}
	return out
}

// sourceSize is the run's source policy's own size, before any clamp: a stored or inline policy's,
// or, when the run takes the caller's profile ceiling as its policy, the profile's AUTHORED size,
// because that spec carries the deployment's size wherever the profile sets none.
func sourceSize(source policySourceRecord, ceiling governanceCeiling) types.ResourceLimits {
	if source.Kind == policyKindProfile {
		return ceiling.authoredSize()
	}
	if r := source.Spec.Resources; r != nil {
		return types.ResourceLimits{CPUMillis: r.CPUMillis, MemoryMiB: r.MemoryMiB}
	}
	return types.ResourceLimits{}
}

// resourceBaseline is governance's untouched size: the resolved spec's size after the policy step,
// which only an admin authored. An inline policy's size is the request's own, so it is never one.
func resourceBaseline(source policySourceRecord, spec types.RunPolicySpec) types.ResourceLimits {
	if source.Kind == policyKindInline || spec.Resources == nil {
		return types.ResourceLimits{}
	}
	return types.ResourceLimits{CPUMillis: spec.Resources.CPUMillis, MemoryMiB: spec.Resources.MemoryMiB}
}

// runSize folds the run's limit sources for its stored run type and applies the ask: the run's limits
// and the size it gets. capacity is the self-hosted candidates' advertised capacity.
func (s *Server) runSize(ctx context.Context, in placementInput, pool *types.RunnerPool, capacity []runnerpool.RunnerCapacity,
) (types.EffectiveRunLimits, client.AppliedResources, *runRefusal) {
	rt := runTypeForEnd(in.req.Experience)
	sources, refusal, err := s.limitSources(ctx, rt, pool, in.ceiling, in.untouched, capacity)
	if refusal != nil {
		return types.EffectiveRunLimits{}, client.AppliedResources{}, poolRefusal(refusal)
	}
	if err != nil {
		return types.EffectiveRunLimits{}, client.AppliedResources{}, runServerError("resolve run limits", err)
	}
	eff, err := runnerpool.Effective(rt, sources...)
	if err != nil {
		return types.EffectiveRunLimits{}, client.AppliedResources{}, runServerError("resolve run limits", err)
	}
	names := map[types.LimitSource]string{}
	if pool != nil {
		names[types.LimitSourcePool] = pool.Name
	}
	if p := in.ceiling.Profile; p != nil {
		names[types.LimitSourceGovernance] = p.Name
	}
	return eff, client.AppliedResources{
		CPUMillis: appliedAmount(eff.CPUMillis, in.ask.CPUMillis, names),
		MemoryMiB: appliedAmount(eff.MemoryMiB, in.ask.MemoryMiB, names),
	}, nil
}

// appliedAmount is one field's ask held against its effective limit, with the source that decided it.
func appliedAmount(l types.EffectiveLimit, asked int, names map[types.LimitSource]string) client.AppliedAmount {
	applied, capped := l.Apply(asked)
	out := client.AppliedAmount{Requested: asked, Applied: applied.Value, Capped: capped}
	switch {
	case capped:
		out.Source = l.CapSource
	case asked <= 0:
		out.Source = l.DefaultSource
	}
	out.SourceName = names[out.Source]
	return out
}

// appliedPids is the process limit the run's source policy asked for held against the one it got after
// the ceiling clamp. The pools fold no PIDs limit, so the cut is the ceiling's alone: the caller's
// governance profile when it set a limit of its own, else the deployment (deploymentPids is the limit a
// profile with none inherits). A profile used as the policy asks nothing of its own; a policy_overlay
// that names resources asks through the overlaid spec, which is where the console's ask arrives.
func appliedPids(source policySourceRecord, ceiling governanceCeiling, applied, deploymentPids int) *client.AppliedAmount {
	asked := 0
	switch {
	case source.overlaid != nil && slices.Contains(source.Overlay, "resources"):
		if r := source.overlaid.Resources; r != nil {
			asked = r.PidsLimit
		}
	case source.Kind != policyKindProfile && source.Spec.Resources != nil:
		asked = source.Spec.Resources.PidsLimit
	}
	out := client.AppliedAmount{Requested: asked, Applied: applied}
	if asked > 0 && applied > 0 && applied < asked {
		out.Capped, out.Source = true, types.LimitSourceDeployment
		if p := ceiling.Profile; p != nil && p.Ceiling.Resources != nil && p.Ceiling.Resources.PidsLimit > 0 &&
			p.Ceiling.Resources.PidsLimit != deploymentPids {
			out.Source, out.SourceName = types.LimitSourceGovernance, p.Name
		}
	}
	return &out
}
