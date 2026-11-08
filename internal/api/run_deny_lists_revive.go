// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A revive does not run the create fold, so the unit deny lists (run_deny_lists.go) are held to the
// run again here: a run created before its owner's profile, or a restriction on the owner, listed a
// stored component or a model provider is refused at its next revive, with the create doors' reasons.
// The same wall as at create: no role is exempt.

// withRevivedDenyLists unions a restriction's deny lists into c, as composition does.
func (c ownerCeiling) withRevivedDenyLists(l types.RestrictionLimits) ownerCeiling {
	if l.DenyComponents != nil {
		c.denyComponents = unionDenyIDs(c.denyComponents, *l.DenyComponents)
	}
	if l.DenyModelProviders != nil {
		c.denyModelProviders = unionDenyIDs(c.denyModelProviders, *l.DenyModelProviders)
	}
	return c
}

// unionDenyIDs is base followed by the ids of more it lacks, in a fresh slice.
func unionDenyIDs(base, more []string) []string {
	out := slices.Clone(base)
	for _, id := range more {
		if !slices.Contains(out, id) {
			out = append(out, id)
		}
	}
	return out
}

// reviveDenyLists refuses a revive of a run that holds a stored component, or a model provider (the
// run's own or an included harness's), that c's lists deny: 403 component_denied or
// model_provider_denied, audited as run.revive denied. A run is judged by what it was launched with
// (its component snapshot and its harness rows); a provider that no longer exists is the owner
// recheck's refusal, not this one.
func (s *Server) reviveDenyLists(ctx context.Context, run types.AgentRun, c ownerCeiling, actorType types.ActorType, actor string) *reviveError {
	if len(c.denyComponents) == 0 && len(c.denyModelProviders) == 0 {
		return nil
	}
	unreadable := func(what string, err error) *reviveError {
		return reviveRefused(http.StatusServiceUnavailable, reasonReviveOwnerAuthorityUnreadable, loggedMsg(ctx, what, err))
	}
	deny := func(reason, msg string) *reviveError {
		s.recordAudit(ctx, s.auditEvent(&run.ID, actorType, actor, "run.revive", run.ID.String(), "denied",
			mustJSON(map[string]any{"subject": run.CreatedBy, "reason": reason})))
		return reviveRefused(http.StatusForbidden, reason, msg)
	}
	comps, err := s.runComponentSnapshot(ctx, run.ID)
	if err != nil {
		return unreadable("read the run's components", err)
	}
	for _, rc := range comps {
		if rc.ComponentID != nil && slices.Contains(c.denyComponents, rc.ComponentID.String()) {
			return deny(reasonComponentDenied, deniedComponentMsg(rc.Name))
		}
	}
	if len(c.denyModelProviders) == 0 {
		return nil
	}
	pairs, err := s.runHarnessPairs(ctx, run)
	if err != nil {
		return unreadable("read the run's harnesses", err)
	}
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		return unreadable("read site config", err)
	}
	for _, h := range pairs {
		if p, found := modelProviderByID(sc.ModelProviders, h.ModelProviderID); found && p.UID != "" && slices.Contains(c.denyModelProviders, p.UID) {
			return deny(reasonModelProviderDenied, deniedProviderMsg(cmp.Or(p.Name, p.ID)))
		}
	}
	return nil
}
