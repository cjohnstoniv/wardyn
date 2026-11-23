// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A session's own binding holds only while its provider, UID, mark, model, grant and credential all hold, whatever
// another provider answers: in every case below provider b stays ready, and availability says so.
func TestAssistantBindingHolds(t *testing.T) {
	newState := func() (*types.AssistantSettings, []types.ModelProvider) {
		set := &types.AssistantSettings{Compose: true, Providers: []types.AssistantProviderMark{
			{ProviderID: "a", Compose: true, Models: []string{"m1", "m2"}}, {ProviderID: "b", Compose: true, Models: []string{"m1"}}}}
		return set, []types.ModelProvider{
			assistantTestProvider("a", "uid-a", types.ModelProviderAnthropicAPIKey, "m1"),
			assistantTestProvider("b", "uid-b", types.ModelProviderAnthropicAPIKey, "m1")}
	}
	bind := types.AssistantBinding{ProviderID: "a", ProviderUID: "uid-a", Model: "m1"}
	never := func(types.ModelProvider) bool { return false }
	all := func(types.ModelProvider) bool { return true }
	notA := func(p types.ModelProvider) bool { return p.ID != "a" }
	cases := []struct {
		name      string
		change    func(*types.AssistantSettings, []types.ModelProvider)
		deny      []string
		allowed   []string
		connected func(types.ModelProvider) bool
		want      string
	}{
		{"healthy", func(*types.AssistantSettings, []types.ModelProvider) {}, nil, nil, all, ""},
		{"purpose off", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Compose = false }, nil, nil, all, reasonAssistantOff},
		{"mark removed", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Providers = s.Providers[1:] }, nil, nil, all, reasonAssistantSessionProviderChanged},
		{"mark stops serving the purpose", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Providers[0].Compose = false }, nil, nil, all, reasonAssistantSessionProviderChanged},
		{"unit denies the provider", func(*types.AssistantSettings, []types.ModelProvider) {}, []string{"uid-a"}, nil, all, reasonAssistantSessionProviderChanged},
		{"model leaves the mark", func(s *types.AssistantSettings, _ []types.ModelProvider) { s.Providers[0].Models = []string{"m2"} }, nil, nil, all, reasonAssistantSessionProviderChanged},
		{"model leaves the person's allowed models", func(*types.AssistantSettings, []types.ModelProvider) {}, nil, []string{"m2"}, all, reasonAssistantSessionProviderChanged},
		{"UID rotated", func(_ *types.AssistantSettings, ps []types.ModelProvider) { ps[0].UID = "uid-rotated" }, nil, nil, all, reasonAssistantSessionProviderChanged},
		{"credential removed", func(*types.AssistantSettings, []types.ModelProvider) {}, nil, nil, notA, reasonAssistantSessionProviderChanged},
		{"provider disabled", func(_ *types.AssistantSettings, ps []types.ModelProvider) { ps[0].Disabled = true }, nil, nil, all, reasonAssistantSessionProviderChanged},
	}
	for _, c := range cases {
		set, ps := newState()
		c.change(set, ps)
		if got := assistantBindingHolds(set, types.AssistantCompose, bind, ps, never, c.deny, c.allowed, c.connected); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
		if c.want == reasonAssistantSessionProviderChanged {
			if ans := assistantAvailability(set, types.AssistantCompose, ps, never, c.deny, c.allowed, c.connected); ans.State != types.AssistantReady || ans.ProviderID != "b" {
				t.Errorf("%s: availability = %+v, want ready on b", c.name, ans)
			}
		}
	}
}
