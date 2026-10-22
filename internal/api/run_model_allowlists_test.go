// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// allowlistRecorder is a store that keeps the run's model allowlist record, as the PG one does.
type allowlistRecorder struct {
	captureGrantStore
	sets  int
	lists map[string][]string
}

func (a *allowlistRecorder) SetRunModelAllowlists(_ context.Context, _ uuid.UUID, lists map[string][]string) error {
	a.sets++
	a.lists = lists
	return nil
}

func (a *allowlistRecorder) RunModelAllowlists(context.Context, uuid.UUID) (map[string][]string, error) {
	return a.lists, nil
}

// #3374: dispatch keeps, on the run, the allowlist it hands the proxy, so the run page can say what the
// run is held to after the provider's own list changes. A run on no local model leaves none.
func TestDispatch_RecordsTheModelAllowlistTheProxyIsHanded(t *testing.T) {
	p := localModelProvider()
	for _, tc := range []struct {
		name     string
		provider types.ModelProvider
		want     map[string][]string
	}{
		{"a local model: its allowlist, by provider", p, map[string][]string{p.ID: p.Local.Models}},
		{"a key provider: none", mpKeyProvider("corp", "uid-corp", types.ModelProviderAnthropicAPIKey, types.ProviderHarness{Harness: "claude-code"}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := &bearerGuardStore{
				run:  types.AgentRun{ID: uuid.New(), Agent: "claude-code", CreatedBy: mpOwner, ModelProviderID: tc.provider.ID},
				site: types.SiteConfig{ModelProviders: providerBlock(tc.provider)},
			}
			h := mpHarness(t, st, tc.provider)
			rec := &allowlistRecorder{lists: map[string][]string{"stale": {"old"}}}
			h.srv.cfg.Store = rec
			policy := types.RunPolicySpec{}
			_, ok := h.srv.resolveLLMInjections(context.Background(), st.run, dispatchParams{}, &policy, map[string]string{}, nil,
				"http://wardyn-proxy:3128", artifactRedirectPlan{}, false, st.site, true, false, bedrockCredUngraded())
			if !ok {
				t.Fatal("dispatch refused the run")
			}
			if rec.sets != 1 || len(rec.lists) != len(tc.want) || (tc.want != nil && !slices.Equal(rec.lists[p.ID], tc.want[p.ID])) {
				t.Fatalf("recorded %d times, %v; want once, %v", rec.sets, rec.lists, tc.want)
			}
		})
	}
}

type allowlistRunStore struct {
	runProviderStore
	lists map[string][]string
}

func (a allowlistRunStore) RunModelAllowlists(context.Context, uuid.UUID) (map[string][]string, error) {
	return a.lists, nil
}

func (allowlistRunStore) SetRunModelAllowlists(context.Context, uuid.UUID, map[string][]string) error {
	return nil
}

// #3374: GET /runs/{id} serves the record to the run's own owner, the member who cannot read GET
// /model-providers, and says nothing on a run that holds none.
func TestGetRun_ServesTheModelAllowlistItWasLaunchedWith(t *testing.T) {
	for _, tc := range []struct {
		name  string
		lists map[string][]string
	}{
		{"recorded", map[string][]string{"laptop-gpu": {"qwen2.5-coder:7b", "llama3.2:3b"}}},
		{"none held", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := allowlistRunStore{runProviderStore: runProviderStore{runTypeStore: runTypeStore{newUIMemStore()}}, lists: tc.lists}
			cfg := baseTestConfig(newHarness(t), st)
			cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
			srv := New(cfg)
			owner := accessSessionOfType(t, "sub-pm", "pat@corp.example", oidc.RoleUser, "portfolio-manager", []string{})
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-pm", State: types.RunRunning, ModelProviderID: "laptop-gpu"}
			st.putRun(run)
			w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), owner, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET run = %d; body=%s", w.Code, w.Body.String())
			}
			var body struct {
				ModelAllowlists map[string][]string `json:"model_allowlists"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if len(body.ModelAllowlists) != len(tc.lists) || !slices.Equal(body.ModelAllowlists["laptop-gpu"], tc.lists["laptop-gpu"]) {
				t.Errorf("model_allowlists = %v, want %v", body.ModelAllowlists, tc.lists)
			}
		})
	}
}
