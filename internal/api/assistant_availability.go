// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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
		if p.Disabled || !m.Serves(purpose) || !p.Serves(types.AssistantHarness) || p.Kind.IsBedrock() || defers(p) {
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
// reads only: the person's ceiling, the stored block and whether they hold a credential for a provider.
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
	_, ceiling, err := s.resolveDispatchCeiling(ctx)
	if err != nil {
		writeServerError(w, r, "resolve governance ceiling", err)
		return
	}
	var deny, allowed []string
	if !ceiling.Operator {
		deny, allowed = ceiling.Limits.DenyModelProviders, modelLimitOf(ceiling).models
	}
	owner := runIdentitySubject(ctx, principalFromRequest(r))
	statusCtx := secretstore.WithPurpose(ctx, secretstore.PurposeStatus)
	connected := func(p types.ModelProvider) bool {
		switch s.providerAccessFor(statusCtx, p, owner).State {
		case modelAccessLive, modelAccessExpiring:
			return true
		}
		return false
	}
	writeJSON(w, http.StatusOK, assistantAvailability(sc.Assistant, purpose, siteProviders(sc), inspectionDeferredFor, deny, allowed, connected))
}
