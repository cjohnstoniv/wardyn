// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"filippo.io/age"
	"github.com/jackc/pgx/v5/pgxpool"

	secretspg "github.com/cjohnstoniv/wardyn/internal/secretstore/pg"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestPG_ProviderPurgeTriggers is rule 8 against a real Postgres, through each
// door a providers block is written by: PUT /model-providers, PUT /site-config,
// and an MDM `wardyn site-config set` (the CLI client, whole document, no
// UIDs). An address change, a kind change and a deletion each remove that
// provider's credentials from every namespace, and the write's audit row
// counts them; an identical re-apply — what a managed laptop does on every
// boot — removes nothing.
func TestPG_ProviderPurgeTriggers(t *testing.T) {
	doors := []struct {
		name, action string
		put          func(t *testing.T, srv *Server, block *types.ModelProviders)
	}{
		{"PUT /model-providers", "model_provider.write", func(t *testing.T, srv *Server, block *types.ModelProviders) {
			raw, _ := json.Marshal(block)
			if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, string(raw)); w.Code != http.StatusOK {
				t.Fatalf("PUT /model-providers = %d; body=%s", w.Code, w.Body.String())
			}
		}},
		{"PUT /site-config", "site_config.write", func(t *testing.T, srv *Server, block *types.ModelProviders) {
			raw, _ := json.Marshal(types.SiteConfig{ModelProviders: block})
			if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(raw)); w.Code != http.StatusOK {
				t.Fatalf("PUT /site-config = %d; body=%s", w.Code, w.Body.String())
			}
		}},
		{"MDM apply", "site_config.write", func(t *testing.T, srv *Server, block *types.ModelProviders) {
			ts := httptest.NewServer(panicFails(t, srv.Handler()))
			defer ts.Close()
			if _, _, _, err := client.New(ts.URL, adminToken).PutSiteConfig(t.Context(), types.SiteConfig{ModelProviders: block}); err != nil {
				t.Fatalf("site-config set: %v", err)
			}
		}},
	}
	cases := []struct {
		name  string
		edit  func(*types.ModelProviders)
		purge []int // indexes of the stored providers whose credentials must go
	}{
		{"an identical re-apply", func(*types.ModelProviders) {}, nil},
		{"an address change", func(b *types.ModelProviders) { b.Providers[0].BaseURL = "https://other.corp.example" }, []int{0}},
		{"a kind change", func(b *types.ModelProviders) { b.Providers[1] = openAIProvider(); b.Providers[1].ID = "anthropic" }, []int{1}},
		{"a deletion", func(b *types.ModelProviders) { b.Providers = b.Providers[:1] }, []int{1}},
	}
	block := func() *types.ModelProviders {
		return providerBlock(endpointProvider(), keyProvider("anthropic", "claude-code"))
	}
	for _, d := range doors {
		for _, tc := range cases {
			t.Run(d.name+"/"+tc.name, func(t *testing.T) {
				srv, audit, pool, sec := newProviderPurgePGHarness(t)
				ctx := context.Background()
				raw, _ := json.Marshal(block())
				if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, string(raw)); w.Code != http.StatusOK {
					t.Fatalf("seed PUT /model-providers = %d; body=%s", w.Code, w.Body.String())
				}
				sc, err := srv.cfg.Store.GetSiteConfig(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var uids []string
				for _, p := range sc.ModelProviders.Providers {
					uids = append(uids, p.UID)
					for _, owner := range []string{"alice", "bob", "carol"} {
						mustPut(t, sec, owner, providerSecretName(p.UID, providerKeyPart))
					}
					mustPut(t, sec, "alice", providerSecretName(p.UID, providerSSOPart))
					mustPut(t, sec, "bob", providerSecretName(p.UID, providerOAuthPart))
					mustPut(t, sec, "carol", providerSecretName(p.UID, providerEntraPart))
				}
				mustPut(t, sec, "", "anthropic-api-key")
				mustPut(t, sec, "alice", "git-pat")

				next := block()
				tc.edit(next)
				d.put(t, srv, next)

				want := 0
				for i, uid := range uids {
					n := 6
					if slices.Contains(tc.purge, i) {
						n, want = 0, want+6
					}
					if got := rowsLike(t, pool, providerSecretPrefix+uid+"-%"); got != n {
						t.Errorf("provider %d holds %d credentials across every namespace after the write, want %d", i, got, n)
					}
				}
				if got := rowsLike(t, pool, "anthropic-api-key") + rowsLike(t, pool, "git-pat"); got != 2 {
					t.Errorf("unrelated credentials after the write = %d rows, want both kept", got)
				}
				if got := lastAuditData(t, audit, d.action)["per_user_credentials_invalidated"]; got != float64(want) {
					t.Errorf("per_user_credentials_invalidated = %v, want %d", got, want)
				}
			})
		}
	}
}

// newProviderPurgePGHarness backs the site config and the secret store with a
// throwaway Postgres; identity, broker and audit stay newHarness's.
func newProviderPurgePGHarness(t *testing.T) (*Server, *recRecorder, *pgxpool.Pool, *secretspg.Store) {
	t.Helper()
	pool := throwawayPGPool(t)
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	sec, err := secretspg.New(pool, id)
	if err != nil {
		t.Fatal(err)
	}
	h := newHarness(t)
	h.srv.cfg.Store = store.NewPG(pool)
	h.srv.cfg.Secrets = sec
	h.srv.router = h.srv.routes()
	return h.srv, h.audit, pool, sec
}

func mustPut(t *testing.T, sec *secretspg.Store, owner, name string) {
	t.Helper()
	if err := sec.For(owner).Put(context.Background(), name, []byte("value-"+owner+"-"+name)); err != nil {
		t.Fatalf("seed %q/%q: %v", owner, name, err)
	}
}

// rowsLike counts the secrets rows, in every namespace, whose name matches a
// SQL LIKE pattern.
func rowsLike(t *testing.T, pool *pgxpool.Pool, pattern string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM secrets WHERE name LIKE $1`, pattern).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestPG_ProviderPurgeAzureFoundry is rule 8 for the azure_foundry kind, through both write doors: an endpoint
// change and a route change remove every person's -entra blob, and a deployment change keeps them.
func TestPG_ProviderPurgeAzureFoundry(t *testing.T) {
	setAzureGate(t, true)
	doors := []struct {
		name string
		put  func(t *testing.T, srv *Server, block *types.ModelProviders)
	}{
		{"PUT /model-providers", func(t *testing.T, srv *Server, block *types.ModelProviders) {
			raw, _ := json.Marshal(block)
			if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, string(raw)); w.Code != http.StatusOK {
				t.Fatalf("PUT /model-providers = %d; body=%s", w.Code, w.Body.String())
			}
		}},
		{"PUT /site-config", func(t *testing.T, srv *Server, block *types.ModelProviders) {
			raw, _ := json.Marshal(types.SiteConfig{ModelProviders: block})
			if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, string(raw)); w.Code != http.StatusOK {
				t.Fatalf("PUT /site-config = %d; body=%s", w.Code, w.Body.String())
			}
		}},
	}
	cases := []struct {
		name  string
		edit  func(*types.ModelProvider)
		purge bool
	}{
		{"an endpoint change", func(p *types.ModelProvider) { p.Azure.Endpoint = "https://other.services.ai.azure.com" }, true},
		{"a route change", func(p *types.ModelProvider) {
			p.Azure.Route = types.AzureRouteOpenAIV1
			p.Harnesses = []types.ProviderHarness{{Harness: "codex-cli", Model: "gpt-5-codex"}}
		}, true},
		{"a deployment change", func(p *types.ModelProvider) {
			p.Harnesses[0].Model, p.Harnesses[0].FastModel = "another-deployment", "another-fast"
		}, false},
	}
	for _, d := range doors {
		for _, tc := range cases {
			t.Run(d.name+"/"+tc.name, func(t *testing.T) {
				srv, _, pool, sec := newProviderPurgePGHarness(t)
				srv.cfg.ADOLoginFacts = func() (string, string, bool) { return "client-id", "tenant-id", true }
				srv.cfg.HostResolver = publicResolver
				raw, _ := json.Marshal(providerBlock(azureProvider(), keyProvider("anthropic", "claude-code")))
				if w := do(t, srv, http.MethodPut, "/api/v1/model-providers", adminToken, string(raw)); w.Code != http.StatusOK {
					t.Fatalf("seed PUT /model-providers = %d; body=%s", w.Code, w.Body.String())
				}
				sc, err := srv.cfg.Store.GetSiteConfig(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				azureUID, keyUID := sc.ModelProviders.Providers[0].UID, sc.ModelProviders.Providers[1].UID
				for _, owner := range []string{"alice", "bob"} {
					mustPut(t, sec, owner, providerSecretName(azureUID, providerEntraPart))
					mustPut(t, sec, owner, providerSecretName(keyUID, providerKeyPart))
				}

				next := providerBlock(azureProvider(), keyProvider("anthropic", "claude-code"))
				tc.edit(&next.Providers[0])
				d.put(t, srv, next)

				want := 2
				if tc.purge {
					want = 0
				}
				if got := rowsLike(t, pool, providerSecretName(azureUID, providerEntraPart)); got != want {
					t.Errorf("-entra blobs after the write = %d, want %d", got, want)
				}
				if got := rowsLike(t, pool, providerSecretName(keyUID, providerKeyPart)); got != 2 {
					t.Errorf("the other provider's credentials after the write = %d, want both kept", got)
				}
			})
		}
	}
}
