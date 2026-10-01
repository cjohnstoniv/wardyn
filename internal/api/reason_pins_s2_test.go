// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSlice2_QueryAndBodyReasons (#656 slice 2) pins the machine-readable
// `reason` on a representative set of setup/site-config/governance/
// people-access refusals that need no store or fixture beyond the bare
// harness — cheap, direct proof the wiring lands on the wire, mirroring
// slice 1's TestListApprovals_QueryParamReasons/TestDecodeAndValidateCreateRun_Reasons.
func TestSlice2_QueryAndBodyReasons(t *testing.T) {
	cases := []struct {
		name        string
		method      string
		path        string
		body        string
		wantStatus  int
		wantReason  string
		withSecrets bool // mountSecretRoutes only mounts /secrets/{name} when cfg.Secrets != nil
	}{
		{name: "GET /access with no OIDC configured", method: http.MethodGet, path: "/api/v1/access",
			wantStatus: http.StatusServiceUnavailable, wantReason: reasonSSONotConfigured},
		{name: "POST /admin/delegates with no delegate store", method: http.MethodPost, path: "/api/v1/admin/delegates",
			body: `{"name":"n","idp_client_id":"c","group":"g"}`, wantStatus: http.StatusNotImplemented, wantReason: reasonDelegationStoreUnavailable},
		{name: "POST /setup/onboarding-complete with no store", method: http.MethodPost, path: "/api/v1/setup/onboarding-complete",
			wantStatus: http.StatusServiceUnavailable, wantReason: reasonSetupOnboardingStoreUnavailable},
		{name: "PUT /secrets/{name} with an invalid name", method: http.MethodPut, path: "/api/v1/secrets/Not_Valid!",
			body: `{"value":"sk-test-a-long-enough-value"}`, wantStatus: http.StatusBadRequest, wantReason: reasonSecretNameInvalid, withSecrets: true},
		{name: "PUT /secrets/{name} with a reserved name", method: http.MethodPut, path: "/api/v1/secrets/wardyn-provider-x",
			body: `{"value":"sk-test-a-long-enough-value"}`, wantStatus: http.StatusForbidden, wantReason: reasonSecretNameReserved, withSecrets: true},
		{name: "DELETE /people/{principal}/ssh-keys with an empty principal", method: http.MethodDelete,
			path: "/api/v1/people/%20/ssh-keys", wantStatus: http.StatusBadRequest, wantReason: reasonSSHKeyAdminPrincipalRequired},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			srv := h.srv
			if tc.withSecrets {
				cfg := baseTestConfig(h, nil)
				cfg.Secrets = &memSecrets{}
				srv = New(cfg)
			}
			w := do(t, srv, tc.method, tc.path, adminToken, tc.body)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
			if got := errorReason(w); got != tc.wantReason {
				t.Errorf("reason = %q, want %q; body=%s", got, tc.wantReason, w.Body.String())
			}
		})
	}
}

// TestCreateAPIToken_FromAPIToken_ReasonLiteral (#656 slice 2 review round
// S4) pins the LITERAL wire reason handleCreateAPIToken writes when an API
// token tries to mint another — not just the Go constant. Calls the handler
// directly
// (bypassing the auth middleware) with a context carrying both a signed-in
// human AND a stamped API-token id, the exact shape apiTokenAuth leaves an
// API-token-authenticated request in.
func TestCreateAPIToken_FromAPIToken_ReasonLiteral(t *testing.T) {
	h := newHarness(t)
	ctx := withAPITokenID(withOIDCHuman(t.Context(), "alice"), uuid.New())
	r := httptest.NewRequest(http.MethodPost, "/api/v1/tokens", strings.NewReader(`{"name":"n"}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.srv.handleCreateAPIToken(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("an API token minting another: status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "api_token_from_api_token" {
		t.Errorf("reason = %q, want the literal \"api_token_from_api_token\"; body=%s", got, w.Body.String())
	}
}

// personMintFakeStore is a minimal in-memory store.PersonStore, so
// TestMintPersonAPIToken_Removed_ReasonLiteral runs with no Postgres.
type personMintFakeStore struct {
	store.Store
	mu     sync.Mutex
	people map[string]types.Person
}

func (s *personMintFakeStore) CreatePerson(_ context.Context, p types.Person) (types.Person, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.people[p.Principal]; ok {
		return existing, false, nil
	}
	s.people[p.Principal] = p
	return p, true, nil
}

func (s *personMintFakeStore) GetPerson(_ context.Context, principal string) (types.Person, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.people[principal]
	if !ok {
		return types.Person{}, store.ErrNotFound
	}
	return p, nil
}

func (s *personMintFakeStore) ListPeople(context.Context) ([]types.Person, error) { return nil, nil }
func (s *personMintFakeStore) MarkPersonSignedIn(context.Context, string, time.Time) error {
	return nil
}

// TestMintPersonAPIToken_Removed_ReasonLiteral pins the LITERAL wire reason
// handleMintPersonAPIToken writes (#1477): the route refuses every caller, so
// even a person with no derivable sign-in answers person_token_mint_removed.
func TestMintPersonAPIToken_Removed_ReasonLiteral(t *testing.T) {
	h := newHarness(t)
	cfg := baseTestConfig(h, &personMintFakeStore{people: map[string]types.Person{
		"pat-sub": {Principal: "pat-sub", Email: "pat@corp.example"},
	}})
	cfg.OIDC = newAccessAuth(t, map[string]string{"admin@corp.example": "admin"}, "", nil, nil)
	srv := New(cfg)
	admin := accessSession(t, "root", "admin@corp.example", oidc.RoleAdmin, []string{})

	w := doSSO(t, srv, http.MethodPost, "/api/v1/people/pat-sub/tokens", admin, `{"name":"ci"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("mint for a person: status = %d, want 403; body=%s", w.Code, w.Body.String())
	}
	if got := errorReason(w); got != "person_token_mint_removed" {
		t.Errorf("reason = %q, want the literal \"person_token_mint_removed\"; body=%s", got, w.Body.String())
	}
}
