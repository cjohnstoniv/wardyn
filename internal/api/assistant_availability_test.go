// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

func assistantTestProvider(id, uid string, kind types.ModelProviderKind, model string) types.ModelProvider {
	return types.ModelProvider{ID: id, UID: uid, Name: "Provider " + id, Kind: kind,
		Harnesses: []types.ProviderHarness{{Harness: types.AssistantHarness, Model: model}}}
}

func assistantTestMark(id string, compose, review bool, models ...string) types.AssistantProviderMark {
	return types.AssistantProviderMark{ProviderID: id, Compose: compose, Review: review, Models: models}
}

func TestAssistantAvailability(t *testing.T) {
	direct := assistantTestProvider("direct", "uid-direct", types.ModelProviderAnthropicAPIKey, "claude-sonnet-x")
	other := assistantTestProvider("other", "uid-other", types.ModelProviderAnthropicAPIKey, "claude-haiku-x")
	gateway := assistantTestProvider("gw", "uid-gw", types.ModelProviderCustomEndpoint, "m")
	gateway.GatewayIdentity = &types.ModelProviderGatewayIdentity{Inspection: types.GatewayInspectionGateway}
	bedrock := assistantTestProvider("bedrock", "uid-bedrock", types.ModelProviderBedrockBearer, "m")
	codexOnly := types.ModelProvider{ID: "codex", UID: "uid-codex", Kind: types.ModelProviderOpenAIAPIKey,
		Harnesses: []types.ProviderHarness{{Harness: "codex-cli"}}}
	providers := []types.ModelProvider{direct, other, gateway, bedrock, codexOnly}
	yes := func(types.ModelProvider) bool { return true }
	no := func(types.ModelProvider) bool { return false }
	onlyDirect := func(p types.ModelProvider) bool { return p.ID == "direct" }
	set := func(mutate func(*types.AssistantSettings)) *types.AssistantSettings {
		a := &types.AssistantSettings{Compose: true, Review: true, NeverSentKinds: []string{"role_mapping"},
			Providers: []types.AssistantProviderMark{assistantTestMark("direct", true, true)}}
		if mutate != nil {
			mutate(a)
		}
		return a
	}

	for _, tc := range []struct {
		name          string
		set           *types.AssistantSettings
		purpose       types.AssistantPurpose
		deny, allowed []string
		connected     func(types.ModelProvider) bool
		wantState     string
		wantReason    string
		wantProvider  string
		wantModel     string
	}{
		{name: "1 nil block is off", set: nil, purpose: types.AssistantCompose, connected: yes, wantState: types.AssistantOff, wantReason: reasonAssistantOff},
		{name: "1 purpose off", set: set(func(a *types.AssistantSettings) { a.Compose = false }), purpose: types.AssistantCompose, connected: yes,
			wantState: types.AssistantOff, wantReason: reasonAssistantOff},
		{name: "1 off wins over a missing provider", set: set(func(a *types.AssistantSettings) { a.Review = false; a.Providers = nil }),
			purpose: types.AssistantReview, connected: no, wantState: types.AssistantOff, wantReason: reasonAssistantOff},
		{name: "1 off wins over a denial", set: set(func(a *types.AssistantSettings) { a.Review = false }), purpose: types.AssistantReview,
			deny: []string{"uid-direct"}, connected: no, wantState: types.AssistantOff, wantReason: reasonAssistantOff},
		{name: "2 no marks", set: set(func(a *types.AssistantSettings) { a.Providers = nil }), purpose: types.AssistantCompose, connected: yes,
			wantState: types.AssistantNoProvider, wantReason: reasonAssistantNoProvider},
		{name: "2 the mark does not serve the purpose", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("direct", false, true)}
		}), purpose: types.AssistantCompose, connected: yes, wantState: types.AssistantNoProvider, wantReason: reasonAssistantNoProvider},
		{name: "2 the provider is gone", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("deleted", true, true)}
		}), purpose: types.AssistantCompose, connected: yes, wantState: types.AssistantNoProvider, wantReason: reasonAssistantNoProvider},
		{name: "2 a deferring provider's mark is not live", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("gw", true, true)}
		}), purpose: types.AssistantCompose, connected: yes, wantState: types.AssistantNoProvider, wantReason: reasonAssistantNoProvider},
		{name: "2 a Bedrock provider's stored mark is not live", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("bedrock", true, true)}
		}), purpose: types.AssistantCompose, connected: yes, wantState: types.AssistantNoProvider, wantReason: reasonAssistantNoProvider},
		{name: "2 a provider without Claude Code is not live", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("codex", true, true)}
		}), purpose: types.AssistantCompose, connected: yes, wantState: types.AssistantNoProvider, wantReason: reasonAssistantNoProvider},
		{name: "3 the only provider is denied", set: set(nil), purpose: types.AssistantCompose, deny: []string{"uid-direct"}, connected: yes,
			wantState: types.AssistantUnitDenies, wantReason: reasonAssistantUnitDenies},
		{name: "3 no mark model is allowed", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("direct", true, true, "claude-sonnet-x")}
		}), purpose: types.AssistantCompose, allowed: []string{"claude-opus-y"}, connected: yes,
			wantState: types.AssistantUnitDenies, wantReason: reasonAssistantUnitDenies},
		{name: "3 the harness model is not allowed", set: set(nil), purpose: types.AssistantCompose, allowed: []string{"claude-opus-y"}, connected: yes,
			wantState: types.AssistantUnitDenies, wantReason: reasonAssistantUnitDenies},
		{name: "4 no credential names the provider to connect", set: set(nil), purpose: types.AssistantCompose, connected: no,
			wantState: types.AssistantNotConnected, wantReason: reasonAssistantNotConnected, wantProvider: "direct"},
		{name: "5 ready on the harness model", set: set(nil), purpose: types.AssistantCompose, connected: yes,
			wantState: types.AssistantReady, wantProvider: "direct", wantModel: "claude-sonnet-x"},
		{name: "5 the first mark model the limits allow", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("direct", true, true, "claude-opus-y", "claude-sonnet-x")}
		}), purpose: types.AssistantReview, allowed: []string{"claude-sonnet-x"}, connected: yes,
			wantState: types.AssistantReady, wantProvider: "direct", wantModel: "claude-sonnet-x"},
		{name: "a denied provider plus an allowed one is ready on the allowed one", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("direct", true, true), assistantTestMark("other", true, true)}
		}), purpose: types.AssistantCompose, deny: []string{"uid-direct"}, connected: yes,
			wantState: types.AssistantReady, wantProvider: "other", wantModel: "claude-haiku-x"},
		{name: "the first connected provider serves, not the first marked", set: set(func(a *types.AssistantSettings) {
			a.Providers = []types.AssistantProviderMark{assistantTestMark("direct", true, true), assistantTestMark("other", true, true)}
		}), purpose: types.AssistantCompose, connected: func(p types.ModelProvider) bool { return p.ID == "other" },
			wantState: types.AssistantReady, wantProvider: "other", wantModel: "claude-haiku-x"},
		{name: "no limits (an operator is handed none)", set: set(nil), purpose: types.AssistantCompose, connected: onlyDirect,
			wantState: types.AssistantReady, wantProvider: "direct", wantModel: "claude-sonnet-x"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := assistantAvailability(tc.set, tc.purpose, providers, inspectionDeferredFor, tc.deny, tc.allowed, tc.connected)
			if got.State != tc.wantState || got.Reason != tc.wantReason || got.ProviderID != tc.wantProvider || got.Model != tc.wantModel {
				t.Fatalf("answer = %+v, want state %s reason %q provider %q model %q", got, tc.wantState, tc.wantReason, tc.wantProvider, tc.wantModel)
			}
			if tc.wantState == types.AssistantReady && !slices.Equal(got.NeverSentKinds, []string{"role_mapping"}) {
				t.Errorf("ready answer's never-sent kinds = %v", got.NeverSentKinds)
			}
			if tc.wantState != types.AssistantReady && (got.Model != "" || got.NeverSentKinds != nil || got.ProviderKind != "") {
				t.Errorf("a state that is not ready carries more than its provider: %+v", got)
			}
			if tc.wantState == types.AssistantOff && (got.ProviderID != "" || got.ProviderName != "") {
				t.Errorf("an off assistant told a member about a provider: %+v", got)
			}
		})
	}
}

// TestAssistantAvailabilityNamesTheProvider: a ready answer carries the provider's id, display name and
// kind; a provider with no name is shown by its id.
func TestAssistantAvailabilityNamesTheProvider(t *testing.T) {
	p := assistantTestProvider("direct", "uid", types.ModelProviderAnthropicAPIKey, "m")
	set := &types.AssistantSettings{Compose: true, Providers: []types.AssistantProviderMark{assistantTestMark("direct", true, false)}}
	yes := func(types.ModelProvider) bool { return true }
	got := assistantAvailability(set, types.AssistantCompose, []types.ModelProvider{p}, inspectionDeferredFor, nil, nil, yes)
	if got.ProviderName != "Provider direct" || got.ProviderKind != string(types.ModelProviderAnthropicAPIKey) {
		t.Errorf("answer = %+v", got)
	}
	p.Name = ""
	if got := assistantAvailability(set, types.AssistantCompose, []types.ModelProvider{p}, inspectionDeferredFor, nil, nil, yes); got.ProviderName != "direct" {
		t.Errorf("unnamed provider shown as %q, want its id", got.ProviderName)
	}
}
