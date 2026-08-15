// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
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

// TestCreatePolicy_UnknownSecretRefFailsAtAuthorTime pins the author-time
// fail-fast for validateWorkspaceSources' sibling reference. A typo'd api_key
// secret name used to save GREEN and then 422 at every launch that referenced the
// policy — the exact failure mode the workspace check was added to prevent, on
// the other referenced resource. Advisory, not the load-bearing gate: the secret
// can be deleted afterwards, so run-create still re-checks.
func TestCreatePolicy_UnknownSecretRefFailsAtAuthorTime(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.Secrets = &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("k")}}
	const body = `{"name":"p","spec":{"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
		`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com","secret_name":"anthropic-api-kye"}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/policies", adminToken, body)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("code = %d, want 422 (unknown secret must fail at author time); body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "secret: ") {
		t.Errorf("error must carry the \"secret: \" prefix, mirroring \"workspace: \": %s", w.Body.String())
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

// TestRedactPolicyForRead is W12-S1-1's read-path belt-and-braces: a policy
// read must never echo a raw llm_inspection secret VALUE back to a caller,
// even though validatePolicySpec already refuses to persist one (defense in
// depth against a migration/direct-DB-edit violating that invariant). Names
// are not sensitive the way values are and survive redaction untouched.
func TestRedactPolicyForRead(t *testing.T) {
	p := types.RunPolicy{
		Spec: types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			LLMInspection: &types.LLMInspectionSpec{
				Mode: "alert", DetectSecrets: true,
				WorkspaceSecretNames:  []string{"prod-db-password"},
				WorkspaceSecretValues: []string{"a-value-that-must-never-be-read-back"},
			},
		},
	}
	got := redactPolicyForRead(p)
	if got.Spec.LLMInspection == nil {
		t.Fatal("llm_inspection dropped entirely; want it kept (only values redacted)")
	}
	if len(got.Spec.LLMInspection.WorkspaceSecretValues) != 1 {
		t.Fatalf("expected values replaced by a one-element count placeholder, got %v", got.Spec.LLMInspection.WorkspaceSecretValues)
	}
	if strings.Contains(got.Spec.LLMInspection.WorkspaceSecretValues[0], "must-never-be-read-back") {
		t.Errorf("W12-S1-1: policy read DTO leaked the raw secret value: %v", got.Spec.LLMInspection.WorkspaceSecretValues)
	}
	if len(got.Spec.LLMInspection.WorkspaceSecretNames) != 1 || got.Spec.LLMInspection.WorkspaceSecretNames[0] != "prod-db-password" {
		t.Errorf("names must survive redaction (only values are sensitive), got %v", got.Spec.LLMInspection.WorkspaceSecretNames)
	}
	// The ORIGINAL policy's own LLMInspection must not be mutated in place.
	if p.Spec.LLMInspection.WorkspaceSecretValues[0] != "a-value-that-must-never-be-read-back" {
		t.Errorf("redactPolicyForRead must not mutate the input policy's own LLMInspection, got %q", p.Spec.LLMInspection.WorkspaceSecretValues[0])
	}

	// A policy with no llm_inspection (or no values) passes through unchanged.
	plain := types.RunPolicy{Spec: types.RunPolicySpec{MinConfinementClass: types.CC1}}
	if got := redactPolicyForRead(plain); got.Spec.LLMInspection != nil {
		t.Errorf("a policy with no llm_inspection must pass through unchanged, got %+v", got.Spec.LLMInspection)
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
