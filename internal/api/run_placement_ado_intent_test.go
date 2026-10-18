// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// T12 (ADO-DESIGN-03): a local run's Azure DevOps intent is the one lane
// dispatch would author, from the same inputs: none for a cross-organisation
// run (so delivery policy cannot refuse it for a credential dispatch never
// authors), one for a single organisation, and a lane that is off leaves a
// token redirect onto its hosts as the redirect's own intent.
func TestLocalADOIntentFollowsTheAggregateLane(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(adoTestOwner, "m@corp.example", oidc.RoleUser))
	intents := func(t *testing.T, sc types.SiteConfig, req createRunRequest, spec types.RunPolicySpec) []placement.CredentialIntent {
		t.Helper()
		srv := &Server{cfg: Config{Store: &adoTestStore{site: sc}, Secrets: &memSecrets{m: map[string][]byte{feedToken: []byte("v")}}}}
		out, _, ref := srv.localCredentialIntents(r, req, spec, runComponents{}, nil, sc)
		if ref != nil {
			t.Fatalf("refused: %+v", ref.body)
		}
		return out
	}
	count := func(in []placement.CredentialIntent, prefix string) int {
		n := 0
		for _, i := range in {
			if strings.HasPrefix(i.Field, prefix) {
				n++
			}
		}
		return n
	}
	ws := types.RunPolicySpec{WorkspaceRepos: []types.WorkspaceRepo{{Repo: adoTestRepo}}}
	if got := intents(t, wideADOSite(), createRunRequest{Repo: adoFabrikamRepo}, ws); count(got, "ProxyConfig.ADOGrant") != 0 {
		t.Errorf("cross-organisation run: intents %+v, want no Azure DevOps intent", got)
	}
	if got := intents(t, wideADOSite(), createRunRequest{Repo: adoTestRepo}, ws); count(got, "ProxyConfig.ADOGrant") != 1 {
		t.Errorf("single-organisation run: intents %+v, want exactly one Azure DevOps intent", got)
	}
	feed := feedSite()
	feed.WorkspaceProviders.Git[0].BaseURLs = []string{"https://dev.azure.com"}
	npm := types.RunPolicySpec{AllowedDomains: []string{"registry.npmjs.org"}}
	if got := intents(t, feed, createRunRequest{Repo: adoFabrikamRepo}, types.RunPolicySpec{AllowedDomains: npm.AllowedDomains, WorkspaceRepos: ws.WorkspaceRepos}); count(got, "ProxyConfig.Injection[egress_redirect:") != 1 {
		t.Errorf("lane off: intents %+v, want the redirect's own intent", got)
	}
	if got := intents(t, feed, createRunRequest{Repo: adoTestRepo}, npm); count(got, "ProxyConfig.Injection[egress_redirect:") != 0 {
		t.Errorf("lane on: intents %+v, want the redirect's token left to the lane", got)
	}
}

// A local run with the AWS CLI is refused at the create-time intent
// step every door runs, not accepted and failed as it starts.
func TestLocalCredentialIntentsRefuseTheAWSCLI(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil).WithContext(operatorCtx(adoTestOwner, "m@corp.example", oidc.RoleUser))
	id := uuid.New()
	comps := runComponents{attached: []attachedComponent{{snapshot: types.RunComponent{ComponentID: &id,
		Definition: types.ComponentDefinition{CLI: &types.ComponentCLI{Kind: types.CLIKindAWS, AWS: &types.CLIAWS{AccountID: "123456789012"}}}}}}}
	srv := &Server{cfg: Config{Store: &adoTestStore{}}}
	_, _, ref := srv.localCredentialIntents(r, createRunRequest{}, types.RunPolicySpec{}, comps, nil, types.SiteConfig{})
	if ref == nil || ref.body.Reason != string(placement.ReasonPlacementCredential) || ref.body.Error != localAWSCLIRefusal {
		t.Fatalf("refusal = %+v, want %s with the AWS CLI sentence", ref, placement.ReasonPlacementCredential)
	}
}
