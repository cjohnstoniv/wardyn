// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/authz"
)

// The unit deny lists: a governance profile names components (by id) and model providers (by uid)
// that the people it covers may not use. A run that attaches a listed component, or would use a
// listed provider, is refused 403 component_denied or model_provider_denied, at create, Review and
// preview, and again when a queued run starts (the fold runs afresh there).
//
// The lists are a hard wall, so no role is exempt: the refusal reads the ceiling alone and never asks
// runUngoverned. An operator whose run stands outside governance (the admin token, or an SSO admin
// while WARDYN_GOVERN_ADMIN_RUNS is off) resolves no profile and so has no list to meet; an admin whose
// runs are governed meets their profile's lists like anyone. Empty lists deny nothing, so an upgrade
// refuses no run. Inline and built-in components carry no id and are never named.

// denyComponent is a stored component a run attaches.
type denyComponent struct {
	id   uuid.UUID
	name string
}

// denyProvider is a model provider a run would use.
type denyProvider struct{ uid, name string }

// denyCandidates is what a run attaches and uses, as the deny lists judge it.
type denyCandidates struct {
	components []denyComponent
	providers  []denyProvider
}

func deniedComponentMsg(name string) string {
	return fmt.Sprintf("Your organisation's governance profile doesn't allow the component %q. Remove it from the run, or ask your admin.", name)
}

func deniedProviderMsg(name string) string {
	return fmt.Sprintf("Your organisation's governance profile doesn't allow the model provider %q. Choose another provider, or ask your admin.", name)
}

// denyListRefusal is the 403 for the first of cand that c's profile lists, with the authz.denied row
// and the lock a governance door carries; nil when none is listed. Components are judged before
// providers.
func (s *Server) denyListRefusal(ctx context.Context, c governanceCeiling, cand denyCandidates) *runRefusal {
	if i := slices.IndexFunc(cand.components, func(x denyComponent) bool {
		return slices.Contains(c.Limits.DenyComponents, x.id.String())
	}); i >= 0 {
		return s.denyListDecision(ctx, c, reasonComponentDenied, "deny_components",
			authz.Deny(authz.ReasonGovernanceUnit, "runs.component", deniedComponentMsg(cand.components[i].name)).WithPolicy(s.ceilingPolicy(ctx, c)))
	}
	if i := slices.IndexFunc(cand.providers, func(x denyProvider) bool {
		return slices.Contains(c.Limits.DenyModelProviders, x.uid)
	}); i >= 0 {
		return s.denyListDecision(ctx, c, reasonModelProviderDenied, "deny_model_providers",
			authz.Deny(authz.ReasonGovernanceUnit, "runs.model_provider", deniedProviderMsg(cand.providers[i].name)).WithPolicy(s.ceilingPolicy(ctx, c)))
	}
	return nil
}

// denyListDecision is the 403 for d, with the lock of the knob that listed it and the profile to ask.
func (s *Server) denyListDecision(ctx context.Context, c governanceCeiling, reason, knob string, d authz.Decision) *runRefusal {
	if c.Unit != nil {
		d = d.OnKnob(s.runLockOf(ctx, c.Unit, knob))
	}
	return runDoorDenied(reason, d)
}

// denyCandidatesOf is the stored components a run attaches and the model providers it chose (the
// main one and each included harness's).
func denyCandidatesOf(comps runComponents, main runProviderChoice, harnesses []harnessChoice) denyCandidates {
	var cand denyCandidates
	for _, a := range comps.attached {
		if id := a.snapshot.ComponentID; id != nil {
			cand.components = append(cand.components, denyComponent{id: *id, name: a.snapshot.Name})
		}
	}
	choices := []runProviderChoice{main}
	for _, h := range harnesses {
		choices = append(choices, h.choice)
	}
	for _, c := range choices {
		if c.chosen && c.provider.UID != "" {
			cand.providers = append(cand.providers, denyProvider{uid: c.provider.UID, name: cmp.Or(c.provider.Name, c.provider.ID)})
		}
	}
	return cand
}

// stepDenyLists refuses a run that attaches a listed component or uses a listed provider. It runs
// right after stepModelProvider, which settles the providers and follows stepComponents.
func (s *Server) stepDenyLists(f *runFold, _ *foldRecorder) bool {
	if len(f.ceiling.Limits.DenyComponents) == 0 && len(f.ceiling.Limits.DenyModelProviders) == 0 {
		return true
	}
	return !s.denyListRefusal(f.r.Context(), f.ceiling, denyCandidatesOf(f.comps, f.mpChoice, f.harnessChoices)).write(s, f.w, f.r)
}
