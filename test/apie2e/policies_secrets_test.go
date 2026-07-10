// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package apie2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestPolicies_CRUD exercises the full policy lifecycle over the SDK:
// create (201) -> get -> list (contains it) -> update -> delete -> get is 404.
// Uses a unique name per test so it is isolated within the shared DB.
func TestPolicies_CRUD(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	name := "apie2e-policy-" + uuid.NewString()
	created, err := h.sdk.CreatePolicy(ctx, client.PolicyRequest{
		Name: name,
		Spec: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com"},
			MinConfinementClass: types.CC2,
		},
	})
	if err != nil {
		t.Fatalf("CreatePolicy: %v", err)
	}
	if created.ID == uuid.Nil || created.Name != name {
		t.Fatalf("created policy = %+v", created)
	}

	got, err := h.sdk.GetPolicy(ctx, created.ID)
	if err != nil {
		t.Fatalf("GetPolicy: %v", err)
	}
	if got.ID != created.ID {
		t.Errorf("GetPolicy id = %s, want %s", got.ID, created.ID)
	}

	all, err := h.sdk.ListPolicies(ctx)
	if err != nil {
		t.Fatalf("ListPolicies: %v", err)
	}
	if !slices.ContainsFunc(all, func(p types.RunPolicy) bool { return p.ID == created.ID }) {
		t.Errorf("ListPolicies missing %s", created.ID)
	}

	// Update: strengthen the policy to CC3 and add a domain.
	updated, err := h.sdk.UpdatePolicy(ctx, created.ID, client.PolicyRequest{
		Name: name,
		Spec: types.RunPolicySpec{
			AllowedDomains:      []string{"api.anthropic.com", "github.com"},
			MinConfinementClass: types.CC3,
		},
	})
	if err != nil {
		t.Fatalf("UpdatePolicy: %v", err)
	}
	if updated.Spec.MinConfinementClass != types.CC3 {
		t.Errorf("updated min class = %q, want CC3", updated.Spec.MinConfinementClass)
	}

	if derr := h.sdk.DeletePolicy(ctx, created.ID); derr != nil {
		t.Fatalf("DeletePolicy: %v", derr)
	}
	_, gerr := h.sdk.GetPolicy(ctx, created.ID)
	assertAPIStatus(t, gerr, http.StatusNotFound)
}

// TestPolicies_RejectsBadSpec asserts a spec with an invalid confinement class
// fails closed with 400 through the SDK (server-side validatePolicySpec).
func TestPolicies_RejectsBadSpec(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, err := h.sdk.CreatePolicy(context.Background(), client.PolicyRequest{
		Name: "apie2e-bad-" + uuid.NewString(),
		Spec: types.RunPolicySpec{MinConfinementClass: "CC9"},
	})
	assertAPIStatus(t, err, http.StatusBadRequest)
}

// TestPolicies_GetUnknown_404 asserts an unknown policy id is 404 over the SDK.
func TestPolicies_GetUnknown_404(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	_, err := h.sdk.GetPolicy(context.Background(), uuid.New())
	assertAPIStatus(t, err, http.StatusNotFound)
}

// TestSecrets_WriteOnlyLifecycle proves the secret value is NEVER readable via
// the API: PUT a value over the SDK, LIST returns the NAME but not the value,
// then DELETE removes it. The value only ever leaves the platform through the
// proxy-injection internal path (covered by TestSecrets_InjectionResolve).
func TestSecrets_WriteOnlyLifecycle(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	name := "apie2e-secret-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	value := "super-secret-" + uuid.NewString()

	if err := h.sdk.SetSecret(ctx, name, value); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	names, err := h.sdk.ListSecrets(ctx)
	if err != nil {
		t.Fatalf("ListSecrets: %v", err)
	}
	if !slices.Contains(names, name) {
		t.Errorf("ListSecrets missing the name %q", name)
	}
	for _, n := range names {
		if n == value {
			t.Fatalf("ListSecrets leaked the VALUE %q", value)
		}
	}
	// Defense-in-depth: the raw HTTP list body must not contain the value either.
	assertSecretListBodyHidesValue(t, h, value)

	if err := h.sdk.DeleteSecret(ctx, name); err != nil {
		t.Fatalf("DeleteSecret: %v", err)
	}
	after, err := h.sdk.ListSecrets(ctx)
	if err != nil {
		t.Fatalf("ListSecrets after delete: %v", err)
	}
	if slices.Contains(after, name) {
		t.Errorf("secret %q still listed after delete", name)
	}
}

// TestSecrets_ReservedNameForbidden asserts a reserved platform-internal secret
// name (wardyn-signing-key) cannot be written via the API: 403 fail-closed.
func TestSecrets_ReservedNameForbidden(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	err := h.sdk.SetSecret(context.Background(), "wardyn-signing-key", "x")
	assertAPIStatus(t, err, http.StatusForbidden)
}

// TestSecrets_InvalidNameBadRequest asserts a syntactically invalid name is 400.
func TestSecrets_InvalidNameBadRequest(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	err := h.sdk.SetSecret(context.Background(), "Bad..Name!", "x")
	assertAPIStatus(t, err, http.StatusBadRequest)
}

// TestSecrets_InjectionResolve drives the internal proxy-injection endpoint
// end-to-end: an auto-mint api_key grant resolves to the FORMATTED secret value
// for a run-token caller (the proxy). This is the ONLY path a secret value
// leaves the platform, and it is run-token-gated, never admin-gated. We:
//
//  1. set the api_key's backing secret over the admin SDK (write-only);
//  2. seed a RUNNING run + an auto-mint api_key grant pointing at that secret;
//  3. mint a run token and GET /api/v1/internal/injection/{grantID} (the proxy's
//     startup mint) — asserting 200 + the formatted value;
//  4. confirm the admin secrets surface still NEVER returns that value.
func TestSecrets_InjectionResolve(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	ctx := context.Background()

	secretName := "apie2e-apikey-" + strings.ReplaceAll(uuid.NewString(), "-", "")
	secretValue := "sk-ant-" + uuid.NewString()
	if err := h.sdk.SetSecret(ctx, secretName, secretValue); err != nil {
		t.Fatalf("SetSecret: %v", err)
	}

	runID := uuid.New()
	seedRun(t, h, runID)
	scope, _ := json.Marshal(map[string]string{
		"host":        "api.anthropic.com",
		"header":      "x-api-key",
		"format":      "%s",
		"secret_name": secretName,
	})
	grantID := seedGrant(t, h, runID, types.GrantSpec{
		Kind:             types.GrantAPIKey,
		Scope:            scope,
		RequiresApproval: false, // auto-mint: the proxy resolves it at startup
		TTLSeconds:       600,
	})

	// Run-token-gated internal call, exactly as the wardyn-proxy sidecar makes it.
	tok := h.mintRunToken(runID)
	resp := h.getJSON(t, "/api/v1/internal/injection/"+grantID.String(), tok)
	if resp.status != http.StatusOK {
		t.Fatalf("injection resolve status = %d, body=%s", resp.status, resp.body)
	}
	var inj struct {
		Host   string `json:"host"`
		Header string `json:"header"`
		Value  string `json:"value"`
		JTI    string `json:"jti"`
	}
	if err := json.Unmarshal([]byte(resp.body), &inj); err != nil {
		t.Fatalf("decode injection resp: %v (body=%s)", err, resp.body)
	}
	if inj.Header != "x-api-key" {
		t.Errorf("injection header = %q, want x-api-key", inj.Header)
	}
	if inj.Value != secretValue {
		t.Errorf("injection value = %q, want the resolved secret", inj.Value)
	}
	if inj.JTI == "" {
		t.Errorf("injection jti is empty; the broker should have minted a jti")
	}

	// The value is STILL never readable through the admin secrets surface.
	assertSecretListBodyHidesValue(t, h, secretValue)

	_, _ = h.pool.Exec(ctx, `DELETE FROM agent_runs WHERE id=$1`, runID)
}

// TestSecrets_InjectionRequiresRunToken asserts the injection endpoint is NOT
// reachable with the admin token (it is run-token-gated, internal audience),
// failing closed with 401. The admin token is not a JWT for the internal
// audience, so internal-auth rejects it.
func TestSecrets_InjectionRequiresRunToken(t *testing.T) {
	h := newHarness(t, harnessOpts{})
	resp := h.getJSON(t, "/api/v1/internal/injection/"+uuid.NewString(), adminToken)
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("admin token on injection: status = %d, want 401 (body=%s)", resp.status, resp.body)
	}
	// No token at all is also 401.
	resp = h.getJSON(t, "/api/v1/internal/injection/"+uuid.NewString(), "")
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("no token on injection: status = %d, want 401", resp.status)
	}
}

// ─── helpers ──────────────────────────────────────────────────────────────────

// assertSecretListBodyHidesValue fetches the raw GET /api/v1/secrets body and
// asserts the secret value does not appear anywhere in it.
func assertSecretListBodyHidesValue(t *testing.T, h *harness, value string) {
	t.Helper()
	resp := h.getJSON(t, "/api/v1/secrets", adminToken)
	if resp.status != http.StatusOK {
		t.Fatalf("list secrets status = %d", resp.status)
	}
	if strings.Contains(resp.body, value) {
		t.Fatalf("secret value leaked in GET /api/v1/secrets body")
	}
}

// rawResponse is a decoded raw HTTP response (status + body string) used for the
// internal endpoints the SDK does not expose (injection-resolve, raw secrets).
type rawResponse struct {
	status int
	body   string
}

// getJSON performs a bearer-authed GET against the live test server and returns
// the status + body. An empty bearer sends no Authorization header.
func (h *harness) getJSON(t *testing.T, path, bearer string) rawResponse {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, h.srv.URL+path, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return rawResponse{status: resp.StatusCode, body: string(raw)}
}
