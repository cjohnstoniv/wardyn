// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

func TestComponentFacts_AzureOwnTokenAtActualDoors(t *testing.T) {
	p := azureRows()[0]
	p.Name = "Approved Azure provider"
	p.Harnesses = []types.ProviderHarness{{Harness: "claude-code", Model: azA3Deployment}}
	srv := providerRunFixture(t, types.SiteConfig{ModelProviders: providerBlock(p)}, &capStore{}, nil)
	member := govSession(t, govMemberSub, []string{"eng"}, false)
	body := `{"agent":"claude-code","task":"facts"}`
	preview := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, policyPreviewPath, member, body))
	if preview.Status != componentUnknown {
		t.Fatal("preview must not read Azure sign-in")
	}
	missing := doSSO(t, srv, http.MethodPost, componentDoors[1], member, body)
	if missing.Code != http.StatusUnprocessableEntity || strings.Contains(missing.Body.String(), `"components"`) {
		t.Fatalf("missing sign-in = %d %s", missing.Code, missing.Body.String())
	}
	blob := adoEntraBlob{RefreshToken: "private-refresh", Scopes: []string{azFoundryScope}, ExpiresAt: time.Now().Add(-time.Hour), TenantID: "private-tenant", ClientID: "private-client", Subject: govMemberSub}
	store := func() {
		raw, err := json.Marshal(blob)
		if err != nil {
			t.Fatal(err)
		}
		if err := srv.cfg.Secrets.For(govMemberSub).Put(context.Background(), providerSecretName(p.UID, providerEntraPart), raw); err != nil {
			t.Fatal(err)
		}
	}
	store()
	ready := decodedAgentFact(t, doSSO(t, srv, http.MethodPost, componentDoors[1], member, body))
	if ready.Status != componentReady || !slices.Equal(ready.Agent.Secrets, []client.AgentSecretFact{{Kind: "token", Owner: "own", Residency: "proxy"}}) || !slices.Equal(ready.Agent.Hosts, []client.AgentHostFact{{Host: azA3Host, Role: "provider"}}) {
		t.Fatalf("Azure lane = %+v", ready)
	}
	raw, err := json.Marshal(ready)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{p.ID, p.UID, "private-refresh", "private-tenant", "private-client", providerSecretName(p.UID, providerEntraPart)} {
		if strings.Contains(string(raw), private) {
			t.Fatalf("Azure fact leaks %q: %s", private, raw)
		}
	}
	blob.DeadAt = time.Now()
	store()
	ended := doSSO(t, srv, http.MethodPost, componentDoors[1], member, body)
	if ended.Code != http.StatusUnprocessableEntity || strings.Contains(ended.Body.String(), `"components"`) {
		t.Fatalf("ended sign-in = %d %s", ended.Code, ended.Body.String())
	}
}
