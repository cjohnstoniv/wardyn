// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestProviderDispatch_StoredModelOnKeyKinds: the Model an admin stores on a codex-cli harness entry
// of a custom endpoint or an OpenAI key reaches the sandbox as WARDYN_CODEX_MODEL; with none stored
// the variable is left unset, so codex keeps its own default.
func TestProviderDispatch_StoredModelOnKeyKinds(t *testing.T) {
	endpoint := func(model string) types.ModelProvider {
		p := mpKeyProvider("corp-gw", "uid-gw", types.ModelProviderCustomEndpoint,
			types.ProviderHarness{Harness: "codex-cli", Path: "/v1", Model: model})
		p.BaseURL = "https://gw.corp.example"
		return p
	}
	openai := func(model string) types.ModelProvider {
		return mpKeyProvider("openai", "uid-o", types.ModelProviderOpenAIAPIKey,
			types.ProviderHarness{Harness: "codex-cli", Model: model})
	}
	for _, tc := range []struct {
		name string
		p    types.ModelProvider
		want string // "" = the variable must be absent
	}{
		{"custom endpoint with a Model", endpoint("gpt-5-codex"), "gpt-5-codex"},
		{"custom endpoint without a Model", endpoint(""), ""},
		{"openai key with a Model", openai("gpt-5-codex"), "gpt-5-codex"},
		{"openai key without a Model", openai(""), ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &bearerGuardStore{
				run:  types.AgentRun{ID: uuid.New(), Agent: "codex-cli", CreatedBy: mpOwner, ModelProviderID: tc.p.ID},
				site: types.SiteConfig{ModelProviders: providerBlock(tc.p)},
			}
			h := mpHarness(t, st, tc.p)
			policy := types.RunPolicySpec{}
			_, env, _, ok := mpDispatch(t, h, st, &policy, nil)
			if !ok {
				t.Fatal("dispatch refused the run")
			}
			got, set := env["WARDYN_CODEX_MODEL"]
			if tc.want == "" && set {
				t.Errorf("WARDYN_CODEX_MODEL = %q, want it unset when no Model is stored", got)
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("WARDYN_CODEX_MODEL = %q, want %q", got, tc.want)
			}
		})
	}
}
