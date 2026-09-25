// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Handler-level ?limit=&offset= / X-Wardyn-Truncated coverage for the six list
// routes #657 pages: GET /secrets, /integrations, /me/ssh-keys, /me/tokens,
// /runs/{id}/grants and /me/capabilities. The four bare-array routes' SQL-level
// LIMIT/OFFSET is separately proven end-to-end against real Postgres in
// internal/store/store_pagination_657_pg_test.go; these tests exercise the
// HANDLER wiring (parseListPage, the store.*Pager type-assert + fallback, and —
// for the three wrapped-object responses — that the wrapper shape survives
// windowing).
//
// Two paths: TestHandleListGrants_Paginated and TestHandleListAPITokens_PagedScope
// run on pgHarness (real Postgres, which implements the scoped pagers), so they
// take the pageFn path; every other test here runs on a fake that does NOT
// implement them, so it takes the fallback (fetch-all + pageWindow) path.
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestHandleListGrants_Paginated seeds a run whose default policy carries
// several eligible grants (each POST /runs mints one CredentialGrant per
// EligibleGrants entry), then walks GET /runs/{id}/grants?limit=&offset=.
func TestHandleListGrants_Paginated(t *testing.T) {
	srv, _ := pgHarness(t)
	// pgHarness's own default policy already carries one EligibleGrants entry;
	// widen it to several distinct grant kinds so one run seeds more than one
	// row to page over.
	srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"ado-pat": []byte("x"), "deploy-key": []byte("y")}}
	srv.cfg.DefaultPolicy.EligibleGrants = []types.GrantSpec{
		{Kind: types.GrantGitHubToken, Scope: json.RawMessage(`{"repos":["acme/widgets"]}`)},
		{Kind: types.GrantGitPAT, Scope: json.RawMessage(`{"host":"dev.azure.com","secret_name":"ado-pat"}`)},
		{Kind: types.GrantSSHKey, Scope: json.RawMessage(`{"host":"github.com","key_secret_ref":"deploy-key"}`)},
	}
	cw := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken, `{"agent":"claude-code","repo":"acme/widgets"}`)
	if cw.Code != http.StatusCreated {
		t.Fatalf("create run: code = %d, want 201; body=%s", cw.Code, cw.Body.String())
	}
	var run types.AgentRun
	if err := json.Unmarshal(cw.Body.Bytes(), &run); err != nil {
		t.Fatalf("decode run: %v", err)
	}

	w := do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/grants?limit=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page []types.CredentialGrant
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2 page len = %d, want 2", len(page))
	}
	if w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Error("X-Wardyn-Truncated = false, want true (3 grants seeded, limit=2)")
	}

	w = do(t, srv, http.MethodGet, "/api/v1/runs/"+run.ID.String()+"/grants?limit=2&offset=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("offset page: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var tail []types.CredentialGrant
	if err := json.Unmarshal(w.Body.Bytes(), &tail); err != nil {
		t.Fatalf("decode tail: %v", err)
	}
	if len(tail) != 1 {
		t.Fatalf("offset=2 page len = %d, want 1 (3 total)", len(tail))
	}
	if w.Header().Get("X-Wardyn-Truncated") == "true" {
		t.Error("X-Wardyn-Truncated = true on the last page, want false")
	}
	// POST /runs mints every grant with one timestamp, so the pages hold three
	// DISTINCT grants only if the ordering breaks that tie.
	ids := map[uuid.UUID]bool{}
	for _, g := range append(page, tail...) {
		ids[g.ID] = true
	}
	if len(ids) != 3 {
		t.Errorf("pages hold %d distinct grants, want 3 (a grant was repeated across pages)", len(ids))
	}
}

// TestHandleListSSHKeys_Paginated seeds sshMemStore with more keys than a
// requested page and asserts the truncation contract, through the FALLBACK
// path (sshMemStore does not implement store.SSHKeysByPrincipalPager).
func TestHandleListSSHKeys_Paginated(t *testing.T) {
	srv, st := sshKeysTestServer(t)
	for i := 0; i < 5; i++ {
		st.putKey(types.SSHPublicKey{
			Fingerprint: fmt.Sprintf("SHA256:fp-%d", i),
			Principal:   adminTokenPrincipal,
			Name:        "key",
			PublicKey:   "ssh-ed25519 AAAAtest",
		})
	}

	w := do(t, srv, http.MethodGet, "/api/v1/me/ssh-keys?limit=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page []types.SSHPublicKey
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2 page len = %d, want 2", len(page))
	}
	if w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Error("X-Wardyn-Truncated = false, want true (5 keys seeded, limit=2)")
	}

	w = do(t, srv, http.MethodGet, "/api/v1/me/ssh-keys?limit=10", adminToken, "")
	var full []types.SSHPublicKey
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full: %v", err)
	}
	if len(full) != 5 {
		t.Fatalf("limit=10 page len = %d, want 5", len(full))
	}
	if w.Header().Get("X-Wardyn-Truncated") == "true" {
		t.Error("X-Wardyn-Truncated = true for a page that fits everything, want false")
	}
}

// TestHandleListAPITokens_Paginated is the same proof for GET /me/tokens,
// through tokenMemStore's fallback path.
func TestHandleListAPITokens_Paginated(t *testing.T) {
	srv, st, _ := apiTokenTestServer(t)
	ctx := t.Context()
	for i := 0; i < 5; i++ {
		if _, err := st.CreateAPIToken(ctx, types.APIToken{
			ID: uuid.New(), Principal: adminTokenPrincipal, Role: "admin", Name: fmt.Sprintf("ci-%d", i),
		}, fmt.Sprintf("wdn_test%d", i)); err != nil {
			t.Fatalf("seed token %d: %v", i, err)
		}
	}

	w := do(t, srv, http.MethodGet, "/api/v1/me/tokens?limit=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page []types.APIToken
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2 page len = %d, want 2", len(page))
	}
	if w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Error("X-Wardyn-Truncated = false, want true (5 tokens seeded, limit=2)")
	}
}

// TestHandleListAPITokens_PagedScope walks GET /me/tokens on real Postgres —
// the APITokensByPrincipalPager (pageFn) path — as one principal while another
// principal also holds tokens, and asserts the pages hold exactly the caller's
// own tokens: a handler that fed the wrong principal into the paged query
// would serve someone else's credentials inventory.
func TestHandleListAPITokens_PagedScope(t *testing.T) {
	srv, pool := pgHarness(t)
	if _, ok := srv.cfg.Store.(store.APITokensByPrincipalPager); !ok {
		t.Fatal("pgHarness store does not implement store.APITokensByPrincipalPager; this test would not reach the pageFn path")
	}
	ctx := t.Context()
	pg := store.NewPG(pool)
	notTruncated := false
	seed := func(principal string, n int) (ids map[uuid.UUID]bool, bearer string) {
		ids = map[uuid.UUID]bool{}
		for i := 0; i < n; i++ {
			raw := apiTokenPrefix + strings.ReplaceAll(uuid.NewString(), "-", "")
			tok, err := pg.CreateAPIToken(ctx, types.APIToken{
				ID: uuid.New(), Principal: principal, Role: oidc.RoleUser,
				GroupsTruncated: &notTruncated, Name: fmt.Sprintf("ci-%d", i),
			}, raw)
			if err != nil {
				t.Fatalf("seed token %d for %s: %v", i, principal, err)
			}
			ids[tok.ID] = true
			bearer = raw
			t.Cleanup(func() { _, _ = pool.Exec(context.Background(), `DELETE FROM api_tokens WHERE id=$1`, tok.ID) })
		}
		return ids, bearer
	}
	caller := "tokens-page-caller-" + uuid.NewString()
	other := "tokens-page-other-" + uuid.NewString()
	mine, bearer := seed(caller, 5)
	theirs, _ := seed(other, 3)

	seen := map[uuid.UUID]int{}
	for off := 0; ; off += 2 {
		w := do(t, srv, http.MethodGet, fmt.Sprintf("/api/v1/me/tokens?limit=2&offset=%d", off), bearer, "")
		if w.Code != http.StatusOK {
			t.Fatalf("offset=%d: code = %d, want 200; body=%s", off, w.Code, w.Body.String())
		}
		var page []types.APIToken
		if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
			t.Fatalf("offset=%d: decode: %v", off, err)
		}
		for _, tok := range page {
			if tok.Principal != caller || theirs[tok.ID] {
				t.Fatalf("offset=%d: page served token %s of principal %q to caller %q", off, tok.ID, tok.Principal, caller)
			}
			seen[tok.ID]++
		}
		if w.Header().Get("X-Wardyn-Truncated") != "true" {
			break
		}
		if off > 10 {
			t.Fatal("X-Wardyn-Truncated never cleared on a 5-token principal")
		}
	}
	if len(seen) != len(mine) {
		t.Fatalf("pages held %d distinct tokens, want the caller's %d", len(seen), len(mine))
	}
	for id := range mine {
		if seen[id] != 1 {
			t.Errorf("caller token %s seen %d times across the pages, want exactly 1", id, seen[id])
		}
	}
}

// TestHandleMeCapabilities_Paginated seeds several deployment-wide ("all"
// subject) capability grants — matching every caller, including the plain
// admin token this test authenticates with — and pages Grants inside the
// wrapped meCapabilitiesResponse body.
func TestHandleMeCapabilities_Paginated(t *testing.T) {
	h := newHarness(t)
	st := &capStore{}
	for i := 0; i < 5; i++ {
		st.grants = append(st.grants, types.CapabilityGrant{
			SubjectType: types.CapabilitySubjectAll,
			Capability:  "egress_host",
			Value:       fmt.Sprintf("host-%d.example", i),
			Effect:      types.CapabilityAllow,
		})
	}
	srv := New(baseTestConfig(h, st))

	w := do(t, srv, http.MethodGet, "/api/v1/me/capabilities?limit=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp meCapabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(resp.Grants) != 2 {
		t.Fatalf("limit=2 grants len = %d, want 2", len(resp.Grants))
	}
	if w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Error("X-Wardyn-Truncated = false, want true (5 grants seeded, limit=2)")
	}

	w = do(t, srv, http.MethodGet, "/api/v1/me/capabilities?limit=10", adminToken, "")
	var full meCapabilitiesResponse
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full: %v", err)
	}
	if len(full.Grants) != 5 {
		t.Fatalf("limit=10 grants len = %d, want 5", len(full.Grants))
	}
	if w.Header().Get("X-Wardyn-Truncated") == "true" {
		t.Error("X-Wardyn-Truncated = true for a page that fits everything, want false")
	}
}

// TestHandleListIntegrations_Paginated seeds SiteConfig.Integrations directly
// (fakeSiteConfigStore) and pages the {"integrations":[...]} wrapper — the
// fetch-all + in-Go window path (there is no DB list query for this route:
// see handleListIntegrations' own doc).
func TestHandleListIntegrations_Paginated(t *testing.T) {
	h := newHarness(t)
	var rows []types.Integration
	for i := 0; i < 5; i++ {
		rows = append(rows, types.Integration{ID: fmt.Sprintf("int-%d", i), Name: fmt.Sprintf("Int %d", i), Kind: "custom"})
	}
	cfg := baseTestConfig(h, &fakeSiteConfigStore{cfg: types.SiteConfig{Integrations: rows}})
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/api/v1/integrations?limit=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var got struct {
		Integrations []SetupIntegration `json:"integrations"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Integrations) != 2 {
		t.Fatalf("limit=2 integrations len = %d, want 2", len(got.Integrations))
	}
	if w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Error("X-Wardyn-Truncated = false, want true (5 integrations seeded, limit=2)")
	}
}

// TestHandleListSecrets_Paginated seeds the operator namespace with more
// secret names than a requested page and asserts both names and mine are
// windowed inside the {"names":[...],"mine":[...]} wrapper. memSecrets.List
// iterates a Go map (no defined order), so this asserts LENGTH and the
// truncation header — the same properties the store-level tests pin — rather
// than which specific names land on which page.
func TestHandleListSecrets_Paginated(t *testing.T) {
	h := newHarness(t)
	seeded := map[string][]byte{}
	for i := 0; i < 5; i++ {
		seeded[fmt.Sprintf("secret-%d", i)] = []byte("long-enough-to-be-masked")
	}
	cfg := baseTestConfig(h, &noGovernanceStore{})
	cfg.Secrets = &memSecrets{m: seeded}
	srv := New(cfg)

	w := do(t, srv, http.MethodGet, "/api/v1/secrets?limit=2", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var page struct {
		Names []string `json:"names"`
		Mine  []string `json:"mine"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(page.Names) != 2 || len(page.Mine) != 2 {
		t.Fatalf("limit=2 names/mine = %d/%d, want 2/2", len(page.Names), len(page.Mine))
	}
	if w.Header().Get("X-Wardyn-Truncated") != "true" {
		t.Error("X-Wardyn-Truncated = false, want true (5 secrets seeded, limit=2)")
	}

	w = do(t, srv, http.MethodGet, "/api/v1/secrets?limit=10", adminToken, "")
	var full struct {
		Names []string `json:"names"`
		Mine  []string `json:"mine"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full: %v", err)
	}
	if len(full.Names) != 5 || len(full.Mine) != 5 {
		t.Fatalf("limit=10 names/mine = %d/%d, want 5/5", len(full.Names), len(full.Mine))
	}
	if w.Header().Get("X-Wardyn-Truncated") == "true" {
		t.Error("X-Wardyn-Truncated = true for a page that fits everything, want false")
	}
}
