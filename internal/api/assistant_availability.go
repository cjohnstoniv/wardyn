// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// availabilityAnswer is GET /me/assistant: whether this person can use the assistant for one purpose and,
// when not, why. An off assistant says nothing about providers.
type availabilityAnswer struct {
	State string `json:"state"`
	// Reason is the wire reason of a state that is not ready; absent when ready.
	Reason string `json:"reason,omitempty"`
	// ProviderID and ProviderName name the provider that serves the person, or, when none is connected,
	// the one to connect.
	ProviderID   string `json:"provider_id,omitempty"`
	ProviderName string `json:"provider_name,omitempty"`
	// ProviderKind, Model and NeverSentKinds are set when ready. NeverSentKinds is absent when none is listed.
	ProviderKind   string   `json:"provider_kind,omitempty"`
	Model          string   `json:"model,omitempty"`
	NeverSentKinds []string `json:"never_sent_kinds,omitempty"`
}

// assistantOffAnswer is the answer of a purpose that is off. It carries nothing else.
var assistantOffAnswer = availabilityAnswer{State: types.AssistantOff, Reason: reasonAssistantOff}

// assistantCandidate is a live mark's provider with the models the person may use on it.
type assistantCandidate struct {
	provider types.ModelProvider
	models   []string
}

// assistantAvailability is the decision, from the first row that matches:
//  1. no block, or the purpose off: off;
//  2. no live mark for the purpose: no_provider;
//  3. every live mark's provider denied to the person, or none with a model their allowed models include: unit_denies;
//  4. no credential for any provider left: not_connected, naming the first;
//  5. otherwise ready, on the first provider with a credential and the first model the person may use.
//
// A mark is live when its provider exists and is on, pairs with the claude-code harness, is not Bedrock,
// does not defer prompt inspection and serves the purpose. denyUIDs and allowedModels are the person's
// limits, nil for an operator; an empty allowedModels binds nobody.
func assistantAvailability(set *types.AssistantSettings, purpose types.AssistantPurpose, providers []types.ModelProvider,
	defers func(types.ModelProvider) bool, denyUIDs, allowedModels []string, connected func(types.ModelProvider) bool) availabilityAnswer {
	if set == nil || !set.On(purpose) {
		return assistantOffAnswer
	}
	live := false
	var usable []assistantCandidate
	for _, m := range set.Normalized().Providers {
		i := slices.IndexFunc(providers, func(p types.ModelProvider) bool { return p.ID == m.ProviderID })
		if i < 0 {
			continue
		}
		p := providers[i]
		if !assistantMarkLive(m, p, purpose, defers) {
			continue
		}
		live = true
		if p.UID != "" && slices.Contains(denyUIDs, p.UID) {
			continue
		}
		models := m.Models
		if len(models) == 0 {
			models = []string{harnessModelOf(p)}
		}
		if len(allowedModels) > 0 {
			models = slices.DeleteFunc(slices.Clone(models), func(x string) bool { return !slices.Contains(allowedModels, x) })
		}
		if len(models) > 0 {
			usable = append(usable, assistantCandidate{provider: p, models: models})
		}
	}
	if !live {
		return availabilityAnswer{State: types.AssistantNoProvider, Reason: reasonAssistantNoProvider}
	}
	if len(usable) == 0 {
		return availabilityAnswer{State: types.AssistantUnitDenies, Reason: reasonAssistantUnitDenies}
	}
	for _, c := range usable {
		if connected(c.provider) {
			return availabilityAnswer{State: types.AssistantReady, ProviderID: c.provider.ID, ProviderName: providerDisplayName(c.provider),
				ProviderKind: string(c.provider.Kind), Model: c.models[0], NeverSentKinds: set.Normalized().NeverSentKinds}
		}
	}
	first := usable[0].provider
	return availabilityAnswer{State: types.AssistantNotConnected, Reason: reasonAssistantNotConnected,
		ProviderID: first.ID, ProviderName: providerDisplayName(first)}
}

// assistantMarkLive is the one definition of a live mark: its provider is on, pairs with the claude-code
// harness, is not Bedrock, does not defer prompt inspection and the mark serves the purpose.
func assistantMarkLive(m types.AssistantProviderMark, p types.ModelProvider, purpose types.AssistantPurpose, defers func(types.ModelProvider) bool) bool {
	return !p.Disabled && m.Serves(purpose) && p.Serves(types.AssistantHarness) && !p.Kind.IsBedrock() && !defers(p)
}

// assistantBindingHolds reports why a session's own binding no longer holds, "" when it does. It never
// consults generic availability: a ready answer on another provider or model authorises nothing here. In order:
// the purpose is still on (reasonAssistantOff); then the provider with the binding's id exists with the same
// UID and is not disabled, a mark for it still serves the purpose, the binding's model is still on that mark
// (the harness model when its list is empty) and, when the person's allowed models are non-empty, in them, the
// person's ceiling does not deny the UID, and the person still holds a credential
// (reasonAssistantSessionProviderChanged). It is assistantAvailability's live-mark predicate, asked of one provider.
func assistantBindingHolds(set *types.AssistantSettings, purpose types.AssistantPurpose, b types.AssistantBinding, providers []types.ModelProvider,
	defers func(types.ModelProvider) bool, denyUIDs, allowedModels []string, connected func(types.ModelProvider) bool) string {
	if set == nil || !set.On(purpose) {
		return reasonAssistantOff
	}
	i := slices.IndexFunc(providers, func(p types.ModelProvider) bool { return p.ID == b.ProviderID })
	if i < 0 || providers[i].UID != b.ProviderUID || b.ProviderUID == "" {
		return reasonAssistantSessionProviderChanged
	}
	p := providers[i]
	j := slices.IndexFunc(set.Normalized().Providers, func(m types.AssistantProviderMark) bool { return m.ProviderID == p.ID })
	if j < 0 || !assistantMarkLive(set.Normalized().Providers[j], p, purpose, defers) {
		return reasonAssistantSessionProviderChanged
	}
	models := set.Normalized().Providers[j].Models
	if len(models) == 0 {
		models = []string{harnessModelOf(p)}
	}
	if !slices.Contains(models, b.Model) || (len(allowedModels) > 0 && !slices.Contains(allowedModels, b.Model)) ||
		slices.Contains(denyUIDs, p.UID) || !connected(p) {
		return reasonAssistantSessionProviderChanged
	}
	return ""
}

// harnessModelOf is the model p's claude-code harness uses, "" when it names none.
func harnessModelOf(p types.ModelProvider) string {
	i := slices.IndexFunc(p.Harnesses, func(h types.ProviderHarness) bool { return h.Harness == types.AssistantHarness })
	if i < 0 {
		return ""
	}
	return p.Harnesses[i].Model
}

// providerDisplayName is the name people see for p: its name, else its id.
func providerDisplayName(p types.ModelProvider) string {
	if p.Name != "" {
		return p.Name
	}
	return p.ID
}

// handleGetMyAssistant is GET /api/v1/me/assistant?purpose=compose|review, open to any signed-in person. It
// reads only: the person's ceiling and provider grants, the stored block and whether they hold a credential
// for a provider.
func (s *Server) handleGetMyAssistant(w http.ResponseWriter, r *http.Request) {
	purpose := types.AssistantPurpose(r.URL.Query().Get("purpose"))
	if !purpose.Valid() {
		writeErrorReason(w, http.StatusBadRequest, reasonAssistantSettingsInvalid, "purpose must be compose or review") // PROPOSED
		return
	}
	ctx := r.Context()
	sc, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if sc.Assistant == nil || !sc.Assistant.On(purpose) { // the pure answer's first row, before any other read
		writeJSON(w, http.StatusOK, assistantOffAnswer)
		return
	}
	lim, err := s.assistantLimitsFor(ctx, sc, principalFromRequest(r))
	if err != nil {
		writeServerError(w, r, "resolve the assistant's limits", err)
		return
	}
	ans := assistantAvailability(sc.Assistant, purpose, siteProviders(sc), inspectionDeferredFor, lim.deny, lim.allowed, lim.connected)
	if ans.State == types.AssistantReady {
		full, err := s.assistantAtRunLimit(ctx, lim.ceiling, principalFromRequest(r), purpose)
		if err != nil {
			writeServerError(w, r, "count the person's runs", err)
			return
		}
		if full {
			ans = availabilityAnswer{State: types.AssistantAtRunLimit, Reason: reasonAssistantAtRunLimit}
		}
	}
	writeJSON(w, http.StatusOK, ans)
}

// assistantLimits is what the person's own ceiling and credentials say about the providers a mark names.
type assistantLimits struct {
	ceiling       governanceCeiling
	deny, allowed []string
	connected     func(types.ModelProvider) bool
}

// assistantLimitsFor reads the person's ceiling and provider grants and builds the credential check, for ctx's
// person: the provider UIDs denied to them (the ceiling's, and every marked provider they are not granted, which
// start would refuse), the models their ceiling allows (nil for an operator), and whether they hold a
// credential for a provider.
func (s *Server) assistantLimitsFor(ctx context.Context, sc types.SiteConfig, principal string) (assistantLimits, error) {
	_, ceiling, err := s.resolveDispatchCeiling(ctx)
	if err != nil {
		return assistantLimits{}, err
	}
	lim := assistantLimits{ceiling: ceiling}
	if !ceiling.Operator {
		lim.deny, lim.allowed = ceiling.Limits.DenyModelProviders, modelLimitOf(ceiling).models
	}
	var marks []types.AssistantProviderMark
	if sc.Assistant != nil {
		marks = sc.Assistant.Providers
	}
	for _, m := range marks {
		p, ok := modelProviderByID(sc.ModelProviders, m.ProviderID)
		if !ok || p.UID == "" {
			continue
		}
		granted, gerr := s.capSeamAllowed(ctx, capModelProvider, p.ID)
		if gerr != nil {
			return assistantLimits{}, gerr
		}
		if !granted {
			lim.deny = append(slices.Clone(lim.deny), p.UID)
		}
	}
	owner := runIdentitySubject(ctx, principal)
	statusCtx := secretstore.WithPurpose(ctx, secretstore.PurposeStatus)
	lim.connected = func(p types.ModelProvider) bool {
		switch s.providerAccessFor(statusCtx, p, owner).State {
		case modelAccessLive, modelAccessExpiring:
			return true
		}
		return false
	}
	return lim, nil
}

// assistantAtRunLimit reports whether the person's max_concurrent_runs is spent, so a new run would be
// refused. A person who already has a live session for the purpose reuses its run when they start again.
func (s *Server) assistantAtRunLimit(ctx context.Context, ceiling governanceCeiling, principal string, purpose types.AssistantPurpose) (bool, error) {
	limit := ceiling.Limits.MaxConcurrentRuns
	if limit <= 0 || ceiling.Unit == nil {
		return false, nil
	}
	if ss, ok := s.cfg.Store.(store.AssistantSessionStore); ok {
		if _, err := ss.LiveAssistantSession(ctx, principal, purpose); err == nil {
			return false, nil
		} else if !errors.Is(err, store.ErrNotFound) {
			return false, err
		}
	}
	active, err := s.cfg.Store.CountActiveRunsBy(ctx, principal)
	if err != nil {
		return false, err
	}
	return active >= limit, nil
}
