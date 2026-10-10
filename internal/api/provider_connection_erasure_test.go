// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/erasure"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func TestProviderCausePG_PersonErasureRemovesHistory(t *testing.T) {
	for _, path := range []string{"owner", "person"} {
		t.Run(path, func(t *testing.T) {
			pair, old, next := changedProviderFixture(t)
			purgeChangedProvider(t, pair.a, old, next)
			if got := pair.b.providerAccessFor(t.Context(), next, "person-a"); got.Cause != "destination_changed" {
				t.Fatalf("before erase: %+v", got)
			}
			if path == "owner" {
				if _, err := secretstore.EraseOwner(t.Context(), pair.a.cfg.Secrets, "person-a"); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := pair.a.erasureOrchestrator(nil).Orchestrate(t.Context(), "person-a", []erasure.Scope{erasure.Credentials}); err != nil {
					t.Fatal(err)
				}
			}
			assertErasedProviderHistory(t, pair, next)
			later := next
			later.BaseURL = "https://later.example"
			purgeChangedProvider(t, pair.a, next, later)
			assertErasedProviderHistory(t, pair, later)
			if got := pair.b.providerAccessFor(t.Context(), later, "person-b"); got.Cause != "destination_changed" {
				t.Fatalf("other owner: %+v", got)
			}
		})
	}
}

func assertErasedProviderHistory(t *testing.T, pair *replicaPair, p types.ModelProvider) {
	t.Helper()
	var count int
	if err := pair.poolA.QueryRow(t.Context(), `SELECT count(*) FROM provider_connection_changes WHERE owner='person-a'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("erased history: %d %v", count, err)
	}
	srv := modelProvidersStatusSrv(t, types.SiteConfig{ModelProviders: providerBlock(p), AgentProviders: agentBlock(types.AgentProvider{ID: "claude-code"})}, &capStore{})
	srv.cfg.Secrets = pair.b.cfg.Secrets
	w := doSSO(t, srv, http.MethodGet, "/api/v1/setup/status", ssoSession(t, "person-a", "person-a@example.com", oidc.RoleUser), "")
	if w.Code != http.StatusOK {
		t.Fatalf("setup/status: %d %s", w.Code, w.Body.String())
	}
	var status SetupStatus
	if err := json.Unmarshal(w.Body.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	if len(status.ProviderAccess) != 1 {
		t.Fatalf("access: %+v", status.ProviderAccess)
	}
	got := status.ProviderAccess[0]
	if got.Cause != "never_connected" || got.ChangedAt != nil || got.NewDestination != "" {
		t.Fatalf("erased identity sees history: %+v", got)
	}
}
