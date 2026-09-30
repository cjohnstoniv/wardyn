// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runProviderStore is runTypeStore that also answers the site config.
type runProviderStore struct {
	runTypeStore
	site    types.SiteConfig
	siteErr error
}

func (s runProviderStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return s.site, s.siteErr
}

// #996: GET /runs/{id} names the run's model provider and says it is gone only
// when it is, whoever is reading — the run page used to guess "(removed)" from
// the viewer's own setup status, which lists only what their agents use.
func TestGetRun_NamesTheRunsModelProvider(t *testing.T) {
	live := types.ModelProvider{ID: "corp", Name: "Corp gateway", Kind: types.ModelProviderAnthropicAPIKey}
	for _, tc := range []struct {
		name        string
		site        types.SiteConfig
		siteErr     error
		provider    string
		snapshot    bool // the run.create row froze a name
		wantName    any
		wantDeleted any
	}{
		{name: "live: its current name, no flag", site: types.SiteConfig{ModelProviders: providerBlock(live)},
			provider: "corp", snapshot: true, wantName: "Corp gateway"},
		{name: "deleted: the frozen name, and the flag", site: types.SiteConfig{ModelProviders: providerBlock()},
			provider: "corp", snapshot: true, wantName: "Corp gateway", wantDeleted: true},
		{name: "deleted, nothing frozen: the flag alone", site: types.SiteConfig{ModelProviders: providerBlock()},
			provider: "corp", wantDeleted: true},
		{name: "the site config is unreadable: nothing is claimed", siteErr: errors.New("db down"), provider: "corp", snapshot: true},
		{name: "no provider on the run: nothing", site: types.SiteConfig{ModelProviders: providerBlock(live)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := runProviderStore{runTypeStore: runTypeStore{newUIMemStore()}, site: tc.site, siteErr: tc.siteErr}
			cfg := baseTestConfig(newHarness(t), st)
			cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
			srv := New(cfg)
			// A member whose own setup status lists no provider at all.
			owner := accessSessionOfType(t, "sub-pm", "pat@corp.example", oidc.RoleUser, "portfolio-manager", []string{})
			run := types.AgentRun{ID: uuid.New(), CreatedBy: "sub-pm", State: types.RunRunning, ModelProviderID: tc.provider}
			st.putRun(run)
			if tc.snapshot {
				data, _ := json.Marshal(map[string]any{"model_provider": map[string]any{"id": "corp", "kind": "anthropic_api_key", "name": "Corp gateway"}})
				st.events = append(st.events, types.AuditEvent{ID: uuid.New(), Time: time.Now(), RunID: &run.ID,
					Action: "run.create", Outcome: "success", Data: data})
			}
			w := doSSO(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String(), owner, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET run = %d; body=%s", w.Code, w.Body.String())
			}
			var body map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatal(err)
			}
			if body["model_provider_name"] != tc.wantName || body["model_provider_deleted"] != tc.wantDeleted {
				t.Errorf("model_provider_name = %v, model_provider_deleted = %v; want %v, %v",
					body["model_provider_name"], body["model_provider_deleted"], tc.wantName, tc.wantDeleted)
			}
		})
	}
}

// The run.create snapshot freezes the provider's name beside its id and kind,
// because the run row keeps the id alone and a deleted provider takes its name
// with it. A provider with no name (the field is optional) freezes none.
func TestCreateRunAuditData_FreezesTheProvidersName(t *testing.T) {
	snap := func(p types.ModelProvider) map[string]any {
		data := createRunAuditData(createRunRequest{Agent: "claude-code"}, nil, types.CC2, types.CC2, "jti", nil,
			types.AutonomyResolution{}, false, runProviderChoice{chosen: true, provider: p})
		return data["model_provider"].(map[string]any)
	}
	got := snap(types.ModelProvider{ID: "corp", Name: "Corp gateway", Kind: types.ModelProviderAnthropicAPIKey})
	if got["id"] != "corp" || got["kind"] != types.ModelProviderAnthropicAPIKey || got["name"] != "Corp gateway" {
		t.Errorf("snapshot = %v, want id, kind and name", got)
	}
	if _, present := snap(types.ModelProvider{ID: "corp", Kind: types.ModelProviderAnthropicAPIKey})["name"]; present {
		t.Error("a provider with no name froze an empty one")
	}
}
