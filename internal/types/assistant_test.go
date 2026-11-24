// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"errors"
	"strings"
	"testing"
)

func assistantProvider(id string, kind ModelProviderKind, harnesses ...string) ModelProvider {
	p := ModelProvider{ID: id, Kind: kind}
	for _, h := range harnesses {
		p.Harnesses = append(p.Harnesses, ProviderHarness{Harness: h})
	}
	return p
}

func TestAssistantNilBlockIsAllOff(t *testing.T) {
	var a AssistantSettings
	if a.On(AssistantCompose) || a.On(AssistantReview) || a.RunHistoryCounts || !a.IsZero() {
		t.Fatalf("zero block = %+v, want every switch off and nothing listed", a)
	}
	n := a.Normalized()
	if n.NeverSentKinds == nil || n.Providers == nil || len(n.NeverSentKinds)+len(n.Providers) != 0 {
		t.Errorf("normalized zero block = %+v, want two empty non-nil lists", n)
	}
	if err := a.Validate(nil, func(string) bool { return false }, func(ModelProvider) bool { return false }); err != nil {
		t.Errorf("zero block refused: %v", err)
	}
}

func TestAssistantValidate(t *testing.T) {
	direct := assistantProvider("direct", ModelProviderAnthropicAPIKey, AssistantHarness)
	deferring := assistantProvider("gw", ModelProviderCustomEndpoint, AssistantHarness)
	bedrock := assistantProvider("bedrock", ModelProviderBedrockBearer, AssistantHarness)
	codexOnly := assistantProvider("codex", ModelProviderOpenAIAPIKey, "codex-cli")
	bedrockCodex := assistantProvider("bedrock-codex", ModelProviderBedrockSSO, "codex-cli")
	providers := []ModelProvider{direct, deferring, bedrock, codexOnly, bedrockCodex}
	known := func(k string) bool { return k == "governance_unit" || k == "role_mapping" }
	defers := func(p ModelProvider) bool { return p.ID == "gw" }
	mark := func(id string, models ...string) AssistantProviderMark {
		return AssistantProviderMark{ProviderID: id, Compose: true, Models: models}
	}

	valid := AssistantSettings{
		Compose: true, RunHistoryCounts: true, NeverSentKinds: []string{"role_mapping", "governance_unit"},
		Providers: []AssistantProviderMark{mark("direct", "claude-sonnet-4-5")},
	}
	if err := valid.Validate(providers, known, defers); err != nil {
		t.Fatalf("a valid block was refused: %v", err)
	}

	for _, tc := range []struct {
		name       string
		set        AssistantSettings
		reason     string
		msgContain string
	}{
		{"unknown provider", AssistantSettings{Providers: []AssistantProviderMark{mark("nope")}}, AssistantReasonSettingsInvalid, "nope"},
		{"duplicate mark", AssistantSettings{Providers: []AssistantProviderMark{mark("direct"), mark("direct")}}, AssistantReasonSettingsInvalid, "twice"},
		{"no claude-code harness", AssistantSettings{Providers: []AssistantProviderMark{mark("codex")}}, AssistantReasonCannotServe, "can't serve the assistant yet"},
		{"bedrock pairing claude-code", AssistantSettings{Providers: []AssistantProviderMark{mark("bedrock")}}, AssistantReasonCannotServe, "Bedrock providers can't serve the assistant"},
		{"bedrock without claude-code", AssistantSettings{Providers: []AssistantProviderMark{mark("bedrock-codex")}}, AssistantReasonCannotServe, "can't serve the assistant yet"},
		{"defers inspection", AssistantSettings{Providers: []AssistantProviderMark{mark("gw")}}, AssistantReasonDefersInspection, "prompt inspection to its gateway"},
		{"bad model name", AssistantSettings{Providers: []AssistantProviderMark{mark("direct", " padded")}}, AssistantReasonSettingsInvalid, "models for provider"},
		{"unknown never-sent kind", AssistantSettings{NeverSentKinds: []string{"no_such_kind"}}, AssistantReasonSettingsInvalid, "no_such_kind"},
		{"duplicate never-sent kind", AssistantSettings{NeverSentKinds: []string{"role_mapping", "role_mapping"}}, AssistantReasonSettingsInvalid, "twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.set.Validate(providers, known, defers)
			var ae *AssistantSettingsError
			if !errors.As(err, &ae) || ae.Reason != tc.reason || !strings.Contains(ae.Msg, tc.msgContain) {
				t.Fatalf("Validate = %v, want reason %s mentioning %q", err, tc.reason, tc.msgContain)
			}
		})
	}
}

func TestAssistantNormalizedIsStable(t *testing.T) {
	a := AssistantSettings{
		NeverSentKinds: []string{"b", "a"},
		Providers:      []AssistantProviderMark{{ProviderID: "z", Models: []string{"m2", "m1"}}, {ProviderID: "a"}},
	}.Normalized()
	if a.NeverSentKinds[0] != "a" || a.Providers[0].ProviderID != "a" || a.Providers[0].Models == nil {
		t.Errorf("normalized = %+v, want sorted lists and non-nil models", a)
	}
	if got := a.Providers[1].Models; got[0] != "m2" || got[1] != "m1" {
		t.Errorf("models = %v, want their order kept", got)
	}
}
