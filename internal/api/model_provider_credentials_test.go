// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// credentialSite is a providers block with UIDs minted, the way a stored one
// always carries them.
func credentialSite(ps ...types.ModelProvider) types.SiteConfig {
	block := normalizeModelProviders(providerBlock(ps...))
	assignModelProviderUIDs(block, nil)
	return types.SiteConfig{ModelProviders: block}
}

func openAIProvider() types.ModelProvider {
	return types.ModelProvider{ID: "openai", Kind: types.ModelProviderOpenAIAPIKey,
		Harnesses: []types.ProviderHarness{{Harness: "codex-cli"}}}
}

// providerRows counts every row, in every namespace, held under uid.
func providerRows(mem *memSecrets, uid string) int {
	memSecretsMu.Lock()
	defer memSecretsMu.Unlock()
	n := 0
	for _, rows := range append([]map[string][]byte{mem.m}, slices.Collect(maps.Values(mem.owned))...) {
		for name := range rows {
			if strings.HasPrefix(name, providerSecretPrefix+uid+"-") {
				n++
			}
		}
	}
	return n
}

func lastAuditData(t *testing.T, audit *recRecorder, action string) map[string]any {
	t.Helper()
	var d map[string]any
	for _, ev := range audit.snapshot() {
		if ev.Action == action {
			d = nil
			if err := json.Unmarshal(ev.Data, &d); err != nil {
				t.Fatal(err)
			}
		}
	}
	if d == nil {
		t.Fatalf("no %s event", action)
	}
	return d
}

// TestProviderCredentialOwnNamespace: a person's key lands in their own
// namespace under the provider's UID — never the operator's, admins included —
// is audited without its value, and is read back only by its owner.
func TestProviderCredentialOwnNamespace(t *testing.T) {
	site := credentialSite(keyProvider("anthropic", "claude-code"), openAIProvider(), ssoProvider())
	uid := site.ModelProviders.Providers[0].UID
	cs := &capStore{grants: []types.CapabilityGrant{{
		SubjectType: types.CapabilitySubjectAll, Capability: capAgent, Value: "codex-cli", Effect: types.CapabilityDeny,
	}}}
	srv := modelProvidersStatusSrv(t, site, cs)
	mem := srv.cfg.Secrets.(*memSecrets)
	audit := srv.cfg.Audit.(*recRecorder)
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
	const key = "sk-ant-member-own-key-0001"

	w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/anthropic/credential", member, `{"value":" `+key+`\n"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
	}
	owners := slices.Collect(maps.Keys(mem.owned))
	if len(owners) != 1 || owners[0] == "" {
		t.Fatalf("namespaces written = %q, want exactly the member's own", owners)
	}
	owner := owners[0]
	if got := string(mem.owned[owner][providerSecretName(uid, providerKeyPart)]); got != key {
		t.Errorf("stored %q, want the trimmed key under the provider's UID", got)
	}
	if len(mem.m) != 0 {
		t.Errorf("the operator namespace holds %v, want nothing", slices.Collect(maps.Keys(mem.m)))
	}
	d := lastAuditData(t, audit, "model_provider.credential.write")
	if d["provider"] != "anthropic" || d["owner"] != owner || len(d) != 2 {
		t.Errorf("model_provider.credential.write = %v, want exactly {provider, owner}", d)
	}
	for _, ev := range audit.snapshot() {
		if strings.Contains(string(ev.Data), key) {
			t.Errorf("%s carries the key", ev.Action)
		}
	}

	t.Run("only its owner reads it back", func(t *testing.T) {
		ctx := context.Background()
		if raw, found, err := srv.ownSecret(ctx, owner, providerSecretName(uid, providerKeyPart)); err != nil || !found || string(raw) != key {
			t.Errorf("owner's read = (%q, %v, %v), want the key", raw, found, err)
		}
		if _, found, _ := srv.ownSecret(ctx, "sub-other", providerSecretName(uid, providerKeyPart)); found {
			t.Error("another person read the member's key")
		}
	})

	t.Run("an admin's own key lands under their subject, not the operator namespace", func(t *testing.T) {
		admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
		w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/anthropic/credential", admin, `{"value":"sk-ant-admin-own-key-0001"}`)
		if w.Code != http.StatusNoContent {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
		if len(mem.m) != 0 || len(mem.owned) != 2 {
			t.Errorf("operator rows %v, namespaces %d — want none and two", slices.Collect(maps.Keys(mem.m)), len(mem.owned))
		}
	})

	t.Run("delete removes only the caller's own", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodDelete, "/api/v1/model-providers/anthropic/credential", member, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d; body=%s", w.Code, w.Body.String())
		}
		if _, ok := mem.owned[owner][providerSecretName(uid, providerKeyPart)]; ok {
			t.Error("the member's key survived their DELETE")
		}
		if providerRows(mem, uid) != 1 {
			t.Error("the member's DELETE reached the admin's key")
		}
		if d := lastAuditData(t, audit, "model_provider.credential.delete"); d["provider"] != "anthropic" || d["owner"] != owner {
			t.Errorf("model_provider.credential.delete = %v", d)
		}
	})

	t.Run("delete needs no grant: a withdrawn agent never strands a key", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodDelete, "/api/v1/model-providers/openai/credential", member, "")
		if w.Code != http.StatusNoContent {
			t.Fatalf("DELETE = %d; body=%s", w.Code, w.Body.String())
		}
	})
}

// TestProviderCredentialRefusals: every refusal writes nothing, anywhere.
func TestProviderCredentialRefusals(t *testing.T) {
	site := credentialSite(keyProvider("anthropic", "claude-code"), openAIProvider(), ssoProvider())
	cs := &capStore{grants: []types.CapabilityGrant{{
		SubjectType: types.CapabilitySubjectAll, Capability: capAgent, Value: "codex-cli", Effect: types.CapabilityDeny,
	}}}
	member := ssoSession(t, "sub-member", "member@corp.example", oidc.RoleMember)
	for _, tc := range []struct {
		name, path, body string
		admin            bool
		want             int
	}{
		{"an unknown provider", "nope", `{"value":"sk-ant-member-own-key-0001"}`, false, http.StatusNotFound},
		{"a provider for an agent the person is denied", "openai", `{"value":"sk-member-own-key-0001"}`, false, http.StatusNotFound},
		{"a sign-in provider", "bedrock-prod", `{"value":"sk-member-own-key-0001"}`, false, http.StatusUnprocessableEntity},
		{"an empty value", "anthropic", `{"value":"   "}`, false, http.StatusBadRequest},
		{"a value too short to mask", "anthropic", `{"value":"short"}`, false, http.StatusBadRequest},
		{"the admin token under OIDC, which is not a person", "anthropic", `{"value":"sk-ant-admin-token-key-01"}`, true, http.StatusUnprocessableEntity},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelProvidersStatusSrv(t, site, cs)
			mem := srv.cfg.Secrets.(*memSecrets)
			path := "/api/v1/model-providers/" + tc.path + "/credential"
			var w *httptest.ResponseRecorder
			if tc.admin {
				w = do(t, srv, http.MethodPut, path, adminToken, tc.body)
			} else {
				w = doSSO(t, srv, http.MethodPut, path, member, tc.body)
			}
			if w.Code != tc.want {
				t.Fatalf("PUT = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
			if len(mem.m) != 0 || len(mem.owned) != 0 {
				t.Errorf("a refused PUT wrote %v / %v", mem.m, mem.owned)
			}
		})
	}

	t.Run("no provider block: the door does not answer", func(t *testing.T) {
		srv := modelProvidersStatusSrv(t, types.SiteConfig{}, &capStore{})
		if w := doSSO(t, srv, http.MethodPut, "/api/v1/model-providers/anthropic/credential", member,
			`{"value":"sk-ant-member-own-key-0001"}`); w.Code != http.StatusNotFound {
			t.Fatalf("PUT = %d, want 404", w.Code)
		}
	})
}

// TestOwnSecretNeverFallsBackToOperator: the strict read answers absent where
// Store.For(owner).Get would serve the operator's row of the same name.
func TestOwnSecretNeverFallsBackToOperator(t *testing.T) {
	srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
	mem := &memSecrets{m: map[string][]byte{"shared-name": []byte("operator-value")}}
	srv.cfg.Secrets = mem
	ctx := context.Background()
	if _, found, err := srv.ownSecret(ctx, "alice", "shared-name"); found || err != nil {
		t.Fatalf("alice with no row of her own read (%v, %v), want absent", found, err)
	}
	if _, found, _ := srv.ownSecret(ctx, "", "shared-name"); found {
		t.Fatal("an empty owner read the operator's row")
	}
	_ = mem.For("alice").Put(ctx, "shared-name", []byte("alice-value"))
	if raw, found, err := srv.ownSecret(ctx, "alice", "shared-name"); !found || err != nil || string(raw) != "alice-value" {
		t.Fatalf("alice's own read = (%q, %v, %v), want alice-value", raw, found, err)
	}
	srv.cfg.Secrets = wedgedSecrets{err: errors.New("store down")}
	if _, found, err := srv.ownSecret(ctx, "alice", "shared-name"); found || err == nil {
		t.Fatalf("a wedged store read (%v, %v), want the error, never absent", found, err)
	}
}

// TestProviderSecretNamesReserved is the reserved-name split: every name at the
// generic secrets API and the broker; the sign-in captures at every sink too;
// a key still resolvable at the injection sink.
func TestProviderSecretNamesReserved(t *testing.T) {
	const uid = "0b6f2c9e-5d7a-4c1b-9a3e-2f8d6b4a1c70"
	for _, part := range []string{providerKeyPart, providerOAuthPart, providerSSOPart} {
		name := providerSecretName(uid, part)
		if !secretsAPIReserved(name) {
			t.Errorf("%s is writable through the generic secrets API", name)
		}
		if !broker.ReservedSecretName(name) {
			t.Errorf("%s is mintable as a git_pat/ssh_key value", name)
		}
		if got, want := sinkReservedSecret(name), part != providerKeyPart; got != want {
			t.Errorf("sinkReservedSecret(%s) = %v, want %v", name, got, want)
		}
	}
	srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{})
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	srv.router = srv.routes()
	w := do(t, srv, http.MethodPut, "/api/v1/secrets/"+providerSecretName(uid, providerKeyPart), adminToken, `{"value":"sk-ant-planted-operator-key"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("operator PUT /secrets/<provider key> = %d, want 403", w.Code)
	}
}

// TestModelProviderWritesPurgeCredentials is rule 8 on both write doors, in
// both directions: an address change, a kind change and a deletion each remove
// every person's credential for that provider and no other's; a save that
// changes none of them removes nothing.
func TestModelProviderWritesPurgeCredentials(t *testing.T) {
	type door struct {
		path, action string
		wrap         func(string) string
	}
	doors := []door{
		{"/api/v1/model-providers", "model_provider.write", func(b string) string { return b }},
		{"/api/v1/site-config", "site_config.write", func(b string) string { return `{"model_providers":` + b + `}` }},
	}
	cases := []struct {
		name  string
		edit  func(*types.ModelProviders)
		purge []int // indexes of stored providers whose credentials must go
	}{
		{"an identical save", func(*types.ModelProviders) {}, nil},
		{"a moved address", func(b *types.ModelProviders) { b.Providers[0].BaseURL = "https://other.corp.example" }, []int{0}},
		{"a moved harness path", func(b *types.ModelProviders) { b.Providers[0].Harnesses[0].Path = "/v2" }, []int{0}},
		{"a renamed provider only", func(b *types.ModelProviders) { b.Providers[0].Name = "Gateway" }, nil},
		{"a kind change", func(b *types.ModelProviders) { b.Providers[1] = openAIProvider(); b.Providers[1].ID = "anthropic" }, []int{1}},
		{"a deletion", func(b *types.ModelProviders) { b.Providers = b.Providers[:1] }, []int{1}},
	}
	for _, d := range doors {
		for _, tc := range cases {
			t.Run(d.action+"/"+tc.name, func(t *testing.T) {
				site := credentialSite(endpointProvider(), keyProvider("anthropic", "claude-code"))
				fake := &fakeSiteConfigStore{cfg: site}
				srv, audit := newSiteConfigHarness(t, fake)
				mem := &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("operator-key")}}
				srv.cfg.Secrets = mem
				ctx := context.Background()
				var uids []string
				for _, p := range site.ModelProviders.Providers {
					uids = append(uids, p.UID)
					for _, owner := range []string{"alice", "bob"} {
						_ = mem.For(owner).Put(ctx, providerSecretName(p.UID, providerKeyPart), []byte("k"))
					}
					_ = mem.For("alice").Put(ctx, providerSecretName(p.UID, providerSSOPart), []byte("s"))
				}
				body := normalizeModelProviders(providerBlock(endpointProvider(), keyProvider("anthropic", "claude-code")))
				tc.edit(body)
				raw, _ := json.Marshal(body)

				if w := do(t, srv, http.MethodPut, d.path, adminToken, d.wrap(string(raw))); w.Code != http.StatusOK {
					t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
				}
				for i, uid := range uids {
					want := 3
					if slices.Contains(tc.purge, i) {
						want = 0
					}
					if got := providerRows(mem, uid); got != want {
						t.Errorf("provider %d holds %d credentials after the save, want %d", i, got, want)
					}
				}
				if _, ok := mem.m["anthropic-api-key"]; !ok {
					t.Error("the purge reached an unrelated operator secret")
				}
				if got := lastAuditData(t, audit, d.action)["per_user_credentials_invalidated"]; got != float64(3*len(tc.purge)) {
					t.Errorf("per_user_credentials_invalidated = %v, want %d", got, 3*len(tc.purge))
				}
			})
		}
	}

	t.Run("a site-config save that does not name the block purges nothing", func(t *testing.T) {
		site := credentialSite(endpointProvider())
		srv, _ := newSiteConfigHarness(t, &fakeSiteConfigStore{cfg: site})
		mem := &memSecrets{m: map[string][]byte{}}
		srv.cfg.Secrets = mem
		uid := site.ModelProviders.Providers[0].UID
		_ = mem.For("alice").Put(context.Background(), providerSecretName(uid, providerKeyPart), []byte("k"))
		if w := do(t, srv, http.MethodPut, "/api/v1/site-config", adminToken, `{"scm_hosts":["github.com"]}`); w.Code != http.StatusOK {
			t.Fatalf("PUT = %d; body=%s", w.Code, w.Body.String())
		}
		if providerRows(mem, uid) != 1 {
			t.Error("an unrelated site-config save purged a credential")
		}
	})

	t.Run("a purge that fails refuses the write", func(t *testing.T) {
		for _, d := range doors {
			fake := &fakeSiteConfigStore{cfg: credentialSite(endpointProvider())}
			srv, _ := newSiteConfigHarness(t, fake)
			srv.cfg.Secrets = wedgedSecrets{err: errors.New("store down")}
			body := normalizeModelProviders(providerBlock(endpointProvider()))
			body.Providers[0].BaseURL = "https://other.corp.example"
			raw, _ := json.Marshal(body)
			if w := do(t, srv, http.MethodPut, d.path, adminToken, d.wrap(string(raw))); w.Code != http.StatusInternalServerError {
				t.Errorf("%s: PUT = %d, want 500", d.path, w.Code)
			}
			if fake.putSeen != nil {
				t.Errorf("%s: the block was saved although its credentials were not purged", d.path)
			}
		}
	})
}
