// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memSecrets is a minimal secretstore.Store for tests.
type memSecrets struct{ m map[string][]byte }

func (s *memSecrets) Name() string { return "mem" }
func (s *memSecrets) Put(_ context.Context, name string, v []byte) error {
	if s.m == nil {
		s.m = map[string][]byte{}
	}
	s.m[name] = v
	return nil
}
func (s *memSecrets) Get(_ context.Context, name string) ([]byte, error) {
	v, ok := s.m[name]
	if !ok {
		// Honor the store contract: absent == ErrNotFound (a real Store wraps this
		// so callers can tell "never stored" from a backend failure).
		return nil, secretstore.ErrNotFound
	}
	return v, nil
}
func (s *memSecrets) Delete(_ context.Context, name string) error { delete(s.m, name); return nil }
func (s *memSecrets) List(_ context.Context) ([]string, error) {
	var out []string
	for k := range s.m {
		out = append(out, k)
	}
	return out, nil
}

func newSecretsHarness(t *testing.T) (*harness, *memSecrets) {
	t.Helper()
	h := newHarness(t)
	sec := &memSecrets{m: map[string][]byte{"anthropic-api-key": []byte("sk-ant-test")}}
	h.srv.cfg.Secrets = sec
	h.srv.router = h.srv.routes() // re-mount with the secret surfaces enabled
	return h, sec
}

// The injection-resolve endpoint returns the FORMATTED secret for an api_key
// grant to a run-token caller (the proxy). Structurally sandbox-unreachable —
// see the handler comment; here we verify behavior + audit.
func TestInternalInjection_ResolvesFormattedSecret(t *testing.T) {
	h, _ := newSecretsHarness(t)
	runID := uuid.New()
	token := h.mintRunToken(t, runID)
	h.broker.minted = broker.Minted{
		Kind: types.GrantAPIKey,
		JTI:  "jti-1",
		Injection: &egress.InjectionRule{
			Host: "api.anthropic.com", Header: "x-api-key",
			SecretName: "anthropic-api-key", Format: "%s",
		},
	}

	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", rr.Code, rr.Body.String())
	}
	var resp injectionResponse
	_ = json.Unmarshal(rr.Body.Bytes(), &resp)
	if resp.Header != "x-api-key" || resp.Value != "sk-ant-test" {
		t.Fatalf("resolved = %+v", resp)
	}
	found := false
	for _, ev := range h.audit.events {
		if ev.Action == "secret.read" && ev.Outcome == "success" && ev.Target == "anthropic-api-key" {
			found = true
		}
	}
	if !found {
		t.Fatal("secret.read audit event missing")
	}
}

func TestInternalInjection_FailsClosed(t *testing.T) {
	h, sec := newSecretsHarness(t)
	runID := uuid.New()
	token := h.mintRunToken(t, runID)

	// Wrong kind => 422.
	h.broker.minted = broker.Minted{Kind: types.GrantGitHubToken, Token: "ghs_x", JTI: "j"}
	if rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, ""); rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("github kind: status = %d, want 422", rr.Code)
	}

	// Missing secret => 424 with a helpful message.
	delete(sec.m, "anthropic-api-key")
	h.broker.minted = broker.Minted{
		Kind:      types.GrantAPIKey,
		JTI:       "j2",
		Injection: &egress.InjectionRule{Host: "api.anthropic.com", Header: "x-api-key", SecretName: "anthropic-api-key"},
	}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusFailedDependency || !strings.Contains(rr.Body.String(), "wardyn secret set") {
		t.Fatalf("missing secret: status = %d body=%s", rr.Code, rr.Body.String())
	}

	// No auth => 401.
	if rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), "", ""); rr.Code != http.StatusUnauthorized {
		t.Fatalf("no auth: status = %d, want 401", rr.Code)
	}
}

// Secret management: write/list/delete only; values never come back; reserved
// platform keys are protected; everything is admin-gated and audited.
func TestSecretsAPI_WriteOnlyLifecycle(t *testing.T) {
	h, _ := newSecretsHarness(t)

	if rr := do(t, h.srv, http.MethodPut, "/api/v1/secrets/my-key", adminToken, `{"value":"sk-value-12345"}`); rr.Code != http.StatusNoContent {
		t.Fatalf("put: %d %s", rr.Code, rr.Body.String())
	}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/secrets", adminToken, "")
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "my-key") || strings.Contains(rr.Body.String(), "sk-value-12345") {
		t.Fatalf("list must contain the name and NEVER the value: %d %s", rr.Code, rr.Body.String())
	}
	if rr := do(t, h.srv, http.MethodPut, "/api/v1/secrets/wardyn-signing-key", adminToken, `{"value":"x"}`); rr.Code != http.StatusForbidden {
		t.Fatalf("reserved name must be 403, got %d", rr.Code)
	}
	if rr := do(t, h.srv, http.MethodPut, "/api/v1/secrets/Bad..Name!", adminToken, `{"value":"x"}`); rr.Code != http.StatusBadRequest {
		t.Fatalf("invalid name must be 400, got %d", rr.Code)
	}
	if rr := do(t, h.srv, http.MethodDelete, "/api/v1/secrets/my-key", adminToken, ""); rr.Code != http.StatusNoContent {
		t.Fatalf("delete: %d", rr.Code)
	}
	if rr := do(t, h.srv, http.MethodPut, "/api/v1/secrets/x", "", `{"value":"v"}`); rr.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated put must 401, got %d", rr.Code)
	}

	wantActions := map[string]bool{"secret.write": false, "secret.delete": false}
	for _, ev := range h.audit.events {
		if _, ok := wantActions[ev.Action]; ok {
			wantActions[ev.Action] = true
		}
	}
	for a, seen := range wantActions {
		if !seen {
			t.Errorf("missing audit action %s", a)
		}
	}
}

// TestSecretsAPI_RejectsShortSecret asserts a user secret shorter than
// secretmask.MinLen is rejected 400 (fail-closed) — the masking/scanning layers
// silently drop sub-MinLen values, so accepting one would falsely imply it is
// protected. A value at exactly MinLen must succeed.
func TestSecretsAPI_RejectsShortSecret(t *testing.T) {
	h, _ := newSecretsHarness(t)

	short := strings.Repeat("a", secretmask.MinLen-1)
	rr := do(t, h.srv, http.MethodPut, "/api/v1/secrets/short-key", adminToken,
		`{"value":"`+short+`"}`)
	if rr.Code != http.StatusBadRequest {
		t.Fatalf("sub-MinLen secret must be 400, got %d %s", rr.Code, rr.Body.String())
	}

	ok := strings.Repeat("a", secretmask.MinLen)
	rr = do(t, h.srv, http.MethodPut, "/api/v1/secrets/ok-key", adminToken,
		`{"value":"`+ok+`"}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("secret at MinLen must succeed (204), got %d %s", rr.Code, rr.Body.String())
	}

	// A GitHub App ID is a short numeric identifier (not maskable credential
	// material), so it is EXEMPT from the MinLen gate — rejecting it would break
	// GitHub App setup via the wizard/CLI. A sub-MinLen github-app-id must 204.
	rr = do(t, h.srv, http.MethodPut, "/api/v1/secrets/github-app-id", adminToken,
		`{"value":"123456"}`)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("short github-app-id must be accepted (204), got %d %s", rr.Code, rr.Body.String())
	}
}

// TestSecretsAPI_ListExcludesReserved asserts the list endpoint NEVER surfaces a
// reserved platform-internal secret name even when the underlying store holds
// one (they back identity/session handling and are not user-managed). This was a
// real leak: the reserved names were previously listable.
func TestSecretsAPI_ListExcludesReserved(t *testing.T) {
	h, sec := newSecretsHarness(t)
	// Seed a reserved key directly in the store (bypassing the write API, which
	// forbids it) plus a normal user key.
	sec.m["wardyn-signing-key"] = []byte("super-secret-signing-key")
	sec.m["user-visible"] = []byte("v")

	rr := do(t, h.srv, http.MethodGet, "/api/v1/secrets", adminToken, "")
	if rr.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rr.Code, rr.Body.String())
	}
	body := rr.Body.String()
	if strings.Contains(body, "wardyn-signing-key") {
		t.Fatalf("reserved name leaked into list: %s", body)
	}
	if !strings.Contains(body, "user-visible") {
		t.Fatalf("user-managed name missing from list: %s", body)
	}
}

// TestInternalInjection_RefusesBedrockResidentSecret asserts U029 at the SINK: even
// if a grant is authored (bypassing the write-time policy guard) naming a resident
// AWS SigV4 credential, handleInternalInjection refuses to resolve it into an
// injectable header value — the same defense-in-depth the signing/session keys get.
func TestInternalInjection_RefusesBedrockResidentSecret(t *testing.T) {
	h, sec := newSecretsHarness(t)
	sec.m["aws-secret-access-key"] = []byte("wJalrXUtnFEMI-super-secret-key")
	runID := uuid.New()
	token := h.mintRunToken(t, runID)
	h.broker.minted = broker.Minted{
		Kind: types.GrantAPIKey,
		JTI:  "jti-bedrock",
		Injection: &egress.InjectionRule{
			Host: "attacker.example", Header: "Authorization",
			SecretName: "aws-secret-access-key", Format: "Bearer %s",
		},
	}
	rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
	if rr.Code != http.StatusForbidden {
		t.Fatalf("resident AWS secret at sink must be 403, got %d body=%s", rr.Code, rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "wJalrXUtnFEMI") {
		t.Fatalf("secret value leaked in refusal body: %s", rr.Body.String())
	}
}

// TestSecretsAPI_ReservesOAuthSentinels asserts U215: the two Anthropic OAuth
// injection sentinels are reserved from the GENERIC secrets API — Put and Delete
// are 403 and List never surfaces them — because storing a value under a name that
// resolves live at the injection sink would be silently shadowed. They remain valid
// as an api_key GRANT (the subscription/managed path), which this guard never touches.
func TestSecretsAPI_ReservesOAuthSentinels(t *testing.T) {
	for _, name := range []string{types.SubscriptionOAuthSecret, types.ManagedOAuthSecret} {
		h, sec := newSecretsHarness(t)
		if rr := do(t, h.srv, http.MethodPut, "/api/v1/secrets/"+name, adminToken, `{"value":"pasted-oauth-token-value"}`); rr.Code != http.StatusForbidden {
			t.Fatalf("put sentinel %q must be 403, got %d", name, rr.Code)
		}
		if rr := do(t, h.srv, http.MethodDelete, "/api/v1/secrets/"+name, adminToken, ""); rr.Code != http.StatusForbidden {
			t.Fatalf("delete sentinel %q must be 403, got %d", name, rr.Code)
		}
		// Seeded directly in the store, the sentinel is still excluded from the list.
		sec.m[name] = []byte("shadow-value")
		rr := do(t, h.srv, http.MethodGet, "/api/v1/secrets", adminToken, "")
		if strings.Contains(rr.Body.String(), name) {
			t.Fatalf("sentinel %q leaked into list: %s", name, rr.Body.String())
		}
	}
}
