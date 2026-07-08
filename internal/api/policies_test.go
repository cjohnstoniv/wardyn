// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// These tests exercise the admin-gated policy routes WITHOUT a Postgres pool.
// They deliberately cover only the paths that fail closed BEFORE any store call:
// auth gating, body/spec validation (400), and id parsing (400). The happy-path
// store round-trip (list/get/create/update/delete against a real DB) is covered
// by the WARDYN_TEST_PG-gated store test (internal/store/store_policy_pg_test.go).

func TestPolicyRoutesRequireAdminAuth(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/policies"},
		{http.MethodPost, "/api/v1/policies"},
		{http.MethodGet, "/api/v1/policies/" + uuid.New().String()},
		{http.MethodPut, "/api/v1/policies/" + uuid.New().String()},
		{http.MethodDelete, "/api/v1/policies/" + uuid.New().String()},
	}
	for _, c := range cases {
		// No token.
		if w := do(t, h.srv, c.method, c.path, "", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no token: code = %d, want 401", c.method, c.path, w.Code)
		}
		// Wrong token.
		if w := do(t, h.srv, c.method, c.path, "wrong", ""); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong token: code = %d, want 401", c.method, c.path, w.Code)
		}
	}
}

func TestCreatePolicyValidation(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name string
		body string
	}{
		{"invalid json", `{not json`},
		{"missing name", `{"spec":{"min_confinement_class":"CC2"}}`},
		{"blank name", `{"name":"   ","spec":{"min_confinement_class":"CC2"}}`},
		{"missing min cc", `{"name":"p","spec":{}}`},
		{"unknown min cc", `{"name":"p","spec":{"min_confinement_class":"CC9"}}`},
		{"unknown grant kind", `{"name":"p","spec":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"weird"}]}}`},
		{"negative ttl", `{"name":"p","spec":{"min_confinement_class":"CC2","eligible_grants":[{"kind":"api_key","ttl_seconds":-1}]}}`},
		{"unknown field (typo)", `{"name":"p","spec":{"min_confinement_class":"CC2","allowd_domains":["x"]}}`},
	}
	for _, c := range cases {
		w := do(t, h.srv, http.MethodPost, "/api/v1/policies", adminToken, c.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("create %q: code = %d, want 400; body=%s", c.name, w.Code, w.Body.String())
		}
	}
}

func TestUpdatePolicyValidation(t *testing.T) {
	h := newHarness(t)
	id := uuid.New().String()
	// Invalid body must 400 before the store is ever touched (no pool wired here).
	if w := do(t, h.srv, http.MethodPut, "/api/v1/policies/"+id,
		adminToken, `{"name":"p","spec":{"min_confinement_class":"CC9"}}`); w.Code != http.StatusBadRequest {
		t.Errorf("update invalid spec: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	// A malformed id is rejected with 400 before any decode/store work.
	if w := do(t, h.srv, http.MethodPut, "/api/v1/policies/not-a-uuid",
		adminToken, `{"name":"p","spec":{"min_confinement_class":"CC2"}}`); w.Code != http.StatusBadRequest {
		t.Errorf("update bad id: code = %d, want 400", w.Code)
	}
}

func TestGetDeletePolicyBadID(t *testing.T) {
	h := newHarness(t)
	// Malformed ids are rejected with 400 before any store call (pool-free).
	if w := do(t, h.srv, http.MethodGet, "/api/v1/policies/not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("get bad id: code = %d, want 400", w.Code)
	}
	if w := do(t, h.srv, http.MethodDelete, "/api/v1/policies/not-a-uuid", adminToken, ""); w.Code != http.StatusBadRequest {
		t.Errorf("delete bad id: code = %d, want 400", w.Code)
	}
}
