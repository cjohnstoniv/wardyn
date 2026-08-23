// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"testing"
)

// decode_strict_test.go pins Workstream A phase 2 (decodeStrict alignment):
// POST /runs and POST /runs/preflight now reject an unknown JSON field the
// same way POST /policies always has (decodeStrict, helpers.go) — a typo'd
// field used to be silently DROPPED instead of surfacing as a 400. Go's
// json.Decoder.DisallowUnknownFields recurses into nested structs on its own,
// so the same swap also catches a typo inside inline_policy with no separate
// wiring.
//
// Deliberately NOT touched: approvals.go's decide-body decode, whose own doc
// comment explains why it must stay lenient (three shipped clients depend on
// the tolerance) — no test for it lives here.

// TestCreateRunAndPreflight_UnknownFieldIs400 covers both doors (POST /runs,
// POST /runs/preflight) and both shapes (a typo'd top-level field, a typo'd
// field nested inside inline_policy) in one table — four cases. The nested
// typo ("allowd_domains") matches TestCreatePolicyValidation's own "unknown
// field (typo)" case (policies_test.go), the existing convention for this
// exact typo.
func TestCreateRunAndPreflight_UnknownFieldIs400(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name, path, body, wantField string
	}{
		{
			name:      "runs: top-level typo",
			path:      "/api/v1/runs",
			body:      `{"agent":"claude-code","tsak":"echo hi"}`,
			wantField: "tsak",
		},
		{
			name: "runs: typo nested in inline_policy",
			path: "/api/v1/runs",
			body: `{"agent":"claude-code","task":"echo hi","inline_policy":{` +
				`"min_confinement_class":"CC2","allowd_domains":["api.anthropic.com"]}}`,
			wantField: "allowd_domains",
		},
		{
			name:      "preflight: top-level typo",
			path:      "/api/v1/runs/preflight",
			body:      `{"agent":"claude-code","tsak":"echo hi"}`,
			wantField: "tsak",
		},
		{
			name: "preflight: typo nested in inline_policy",
			path: "/api/v1/runs/preflight",
			body: `{"agent":"claude-code","task":"echo hi","inline_policy":{` +
				`"min_confinement_class":"CC2","allowd_domains":["api.anthropic.com"]}}`,
			wantField: "allowd_domains",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := do(t, h.srv, http.MethodPost, c.path, adminToken, c.body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("code = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), c.wantField) {
				t.Errorf("error must name the unknown field %q; body=%s", c.wantField, w.Body.String())
			}
		})
	}
}

// TestCreateRun_ValidBodyStillSucceeds is decodeStrict's other half: rejecting
// an unknown field must never start rejecting a legitimate, fully-spelled one.
// Postgres-backed (pgHarnessWithRunner): a real 201 needs a real store
// round-trip; skips cleanly without WARDYN_TEST_PG, same as every other
// create-run HTTP test in this package (e.g. task_mode_test.go).
func TestCreateRun_ValidBodyStillSucceeds(t *testing.T) {
	fr := &fakeRunner{}
	srv, _ := pgHarnessWithRunner(t, fr)
	w := do(t, srv, http.MethodPost, "/api/v1/runs", adminToken,
		`{"agent":"claude-code","task":"echo hi"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("valid create-run body: code = %d, want 201; body=%s", w.Code, w.Body.String())
	}
}

// TestPreflight_ValidBodyStillSucceeds is preflight's half of the same guard.
// Body matches TestPreflight_HappyPath (preflight_test.go) — a fully
// credentialed claude-code run — so a nested inline_policy with every field
// correctly spelled must still 200 under the strict decoder. No PG needed:
// preflight persists nothing.
func TestPreflight_ValidBodyStillSucceeds(t *testing.T) {
	h, _ := newSecretsHarness(t) // memSecrets seeded with "anthropic-api-key"
	body := `{"agent":"claude-code","repo":"ephemeral","inline_policy":{` +
		`"min_confinement_class":"CC2","allowed_domains":["api.anthropic.com"],` +
		`"eligible_grants":[{"kind":"api_key","scope":{"host":"api.anthropic.com",` +
		`"header":"x-api-key","format":"%s","secret_name":"anthropic-api-key"}}]}}`
	w := do(t, h.srv, http.MethodPost, "/api/v1/runs/preflight", adminToken, body)
	if w.Code != http.StatusOK {
		t.Fatalf("valid preflight body: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
}
