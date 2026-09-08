// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/broker"
	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/subscription"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// memSecrets is a minimal secretstore.Store for tests. m is the OPERATOR
// namespace (owner == "") — every existing literal in this package
// (&memSecrets{m: ...}) keeps behaving exactly as before For existed. owned
// holds every non-"" owner's rows, lazily allocated by For so every view
// derived from the same root shares it.
type memSecrets struct {
	owner string
	m     map[string][]byte
	owned map[string]map[string][]byte
}

func (s *memSecrets) Name() string { return "mem" }
func (s *memSecrets) Put(_ context.Context, name string, v []byte) error {
	if s.owner == "" {
		if s.m == nil {
			s.m = map[string][]byte{}
		}
		s.m[name] = v
		return nil
	}
	if s.owned[s.owner] == nil {
		s.owned[s.owner] = map[string][]byte{}
	}
	s.owned[s.owner][name] = v
	return nil
}
func (s *memSecrets) Get(_ context.Context, name string) ([]byte, error) {
	if s.owner != "" {
		if v, ok := s.owned[s.owner][name]; ok {
			return v, nil
		}
		// Fall through to the operator row below — the owner-view fallback.
	}
	v, ok := s.m[name]
	if !ok {
		// Honor the store contract: absent == ErrNotFound (a real Store wraps this
		// so callers can tell "never stored" from a backend failure).
		return nil, secretstore.ErrNotFound
	}
	return v, nil
}
func (s *memSecrets) Delete(_ context.Context, name string) error {
	if s.owner == "" {
		delete(s.m, name)
		return nil
	}
	delete(s.owned[s.owner], name)
	return nil
}
func (s *memSecrets) List(_ context.Context) ([]string, error) {
	src := s.m
	if s.owner != "" {
		src = s.owned[s.owner]
	}
	var out []string
	for k := range src {
		out = append(out, k)
	}
	return out, nil
}

// For returns an owner-scoped view sharing the same backing maps as s — see
// secretstore.Store.For's doc comment for the fallback/isolation contract
// this mirrors.
func (s *memSecrets) For(owner string) secretstore.Store {
	if s.owned == nil {
		s.owned = map[string]map[string][]byte{}
	}
	return &memSecrets{owner: owner, m: s.m, owned: s.owned}
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

// TestInternalInjection_RejectsSplittingHeaderName pins the SINK guard: the
// header name a grant authored is written verbatim onto a forwarded request by
// the proxy, so one carrying CR/LF is a header-splitting shape. Every write
// boundary rejects it first (validateIntegrationWrite,
// TestValidatePolicySpec_RejectsSplittingApiKeyHeader) — this covers the grant
// that was RECORDED or written before those guards existed. Fails closed BEFORE
// the secret is read, and audits the refusal.
func TestInternalInjection_RejectsSplittingHeaderName(t *testing.T) {
	for _, bad := range []string{"X-Tok\r\nX-Evil: 1", "X-Tok\nX-Evil: 1", "Authorization: Bearer", ""} {
		h, _ := newSecretsHarness(t)
		token := h.mintRunToken(t, uuid.New())
		h.broker.minted = broker.Minted{
			Kind: types.GrantAPIKey,
			JTI:  "jti-bad-header",
			Injection: &egress.InjectionRule{
				Host: "api.anthropic.com", Header: bad,
				SecretName: "anthropic-api-key", Format: "%s",
			},
		}
		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
		if rr.Code != http.StatusForbidden {
			t.Errorf("header %q: status = %d, want 403; body=%s", bad, rr.Code, rr.Body.String())
		}
		// The secret must not have been read on the way to refusing.
		for _, ev := range h.audit.events {
			if ev.Action == "secret.read" && ev.Outcome == "success" {
				t.Errorf("header %q: secret was read despite the refusal", bad)
			}
		}
		if ev := lastAuditEvent(t, h.audit.events, "secret.read"); !strings.Contains(string(ev.Data), "invalid-header-name") {
			t.Errorf("header %q: audit data = %s, want the invalid-header-name reason", bad, ev.Data)
		}
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

// TestInternalInjection_RefusesBedrockResidentSecret asserts at the SINK: even
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

// TestSecretsAPI_ReservesOAuthSentinels asserts the two Anthropic OAuth
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

// mintRunTokenAs is mintRunToken with a caller-chosen Sub, for owner-scoping
// tests that need to control which principal a run belongs to (mintRunToken
// itself hardcodes "alice@example.com" for every other test in this file).
func mintRunTokenAs(t *testing.T, h *harness, runID uuid.UUID, sub string) string {
	t.Helper()
	id, err := h.idp.MintRunIdentity(context.Background(), runID, sub, "", internalAudience)
	if err != nil {
		t.Fatalf("mint run identity: %v", err)
	}
	return id.Token
}

// TestInjectionResolve_OwnerRowWins_OperatorFallback_OtherOwnerUnreachableEvenWhenNamed
// is invariant 1 (0.7, migration 0050, member BYOK) end to end through the
// REAL handler: a run resolves ITS OWN owner's row when one exists, the
// operator's when it does not, and never another owner's — even though every
// run below names the EXACT SAME secret.
func TestInjectionResolve_OwnerRowWins_OperatorFallback_OtherOwnerUnreachableEvenWhenNamed(t *testing.T) {
	h, sec := newSecretsHarness(t) // seeds the operator's "anthropic-api-key" = "sk-ant-test"
	if err := sec.For("alice").Put(context.Background(), "anthropic-api-key", []byte("sk-ant-alice")); err != nil {
		t.Fatalf("seed alice's row: %v", err)
	}
	// bob owns nothing of his own.

	resolve := func(sub string) string {
		t.Helper()
		runID := uuid.New()
		token := mintRunTokenAs(t, h, runID, sub)
		h.broker.minted = broker.Minted{
			Kind: types.GrantAPIKey,
			JTI:  "jti-" + sub,
			Injection: &egress.InjectionRule{
				Host: "api.anthropic.com", Header: "x-api-key",
				SecretName: "anthropic-api-key", Format: "%s",
			},
		}
		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("sub=%q: status = %d body=%s", sub, rr.Code, rr.Body.String())
		}
		var resp injectionResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &resp)
		return resp.Value
	}

	if got := resolve("alice"); got != "sk-ant-alice" {
		t.Errorf("alice's run resolved %q, want her own row", got)
	}
	// "unreachable even when named": bob's grant above names the EXACT SAME
	// secret name alice owns a row under, and still never reaches it.
	if got := resolve("bob"); got != "sk-ant-test" {
		t.Errorf("bob's run resolved %q, want the operator's fallback (he owns none, and must never see alice's)", got)
	}
}

// TestInjectionResolve_OperatorRunUnchanged is the negative control for the
// test above: an operator-created run (Sub shaped like actorFromRequest's
// admin-token principal, a string secretOwnerFromRequest NEVER writes a row
// under) resolves the operator's secret exactly as every run did before
// Store.For existed.
func TestInjectionResolve_OperatorRunUnchanged(t *testing.T) {
	h, _ := newSecretsHarness(t) // operator's "anthropic-api-key" = "sk-ant-test"
	runID := uuid.New()
	token := mintRunTokenAs(t, h, runID, adminTokenPrincipal)
	h.broker.minted = broker.Minted{
		Kind: types.GrantAPIKey,
		JTI:  "jti-operator",
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
	if resp.Value != "sk-ant-test" {
		t.Fatalf("operator run resolved %q, want the operator's own value unchanged", resp.Value)
	}
}

// ---- F133 pins: the H2 OAuth host pin (both halves) and the sentinel success path ----
//
// handleInternalInjection's OAuth HOST PIN (H2) and its write-time sibling in
// validateInlineSecretRefs were both unheld: short-circuiting either comparison
// left `go test ./internal/api/` fully green, and the coverage profile showed the
// whole sentinel SUCCESS path (forced Bearer clamp, mask registration, 200
// response) at count=0. Three of the four sink chokepoints in that function had a
// named pin; the one guarding exfiltration of a LIVE operator OAuth token had none.
//
// Every case below configures a WORKING sentinel resolve (posture ok, provider
// wired) and changes only the host, so the refusal it asserts can come from the
// host pin and nothing else.

// liveOAuthProvider is a subscription.Provider that yields a LIVE-shaped token
// with a machine-readable expiry, so the success path's expiry advertisement and
// mask registration are both observable.
type liveOAuthProvider struct {
	value   string
	expires time.Time
}

func (p liveOAuthProvider) Current(context.Context) (subscription.Token, error) {
	return subscription.Token{Value: p.value, ExpiresAt: p.expires}, nil
}
func (p liveOAuthProvider) Peek() (subscription.Token, error) {
	return subscription.Token{Value: p.value, ExpiresAt: p.expires}, nil
}

// sentinelHarness wires a harness whose sentinel resolve WOULD succeed: posture
// ok, both providers live, a mask registry to observe. Only the injection rule
// differs per case.
func sentinelHarness(t *testing.T, tok liveOAuthProvider) *harness {
	t.Helper()
	h, _ := newSecretsHarness(t)
	h.srv.cfg.SubscriptionPostureOK = true
	h.srv.cfg.SubscriptionPostureReason = ""
	h.srv.cfg.SubscriptionToken = tok
	h.srv.cfg.ManagedToken = tok
	h.srv.cfg.MaskRegistry = secretmask.NewRegistry()
	return h
}

// TestInternalInjection_RefusesSentinelForNonAnthropicHost is the SINK half of
// the H2 host pin: a grant (authored, inline, or RECORDED before the write-time
// guard existed) that points a sentinel at any other host would exfiltrate the
// operator's live OAuth token, in cleartext on a plain-HTTP allowlist entry.
func TestInternalInjection_RefusesSentinelForNonAnthropicHost(t *testing.T) {
	const live = "oauth-live-token-value"
	for _, sentinel := range []string{types.SubscriptionOAuthSecret, types.ManagedOAuthSecret} {
		for _, host := range []string{"evil.attacker.example", "api.anthropic.com.evil.example", "localhost"} {
			h := sentinelHarness(t, liveOAuthProvider{value: live})
			runID := uuid.New()
			token := h.mintRunToken(t, runID)
			h.broker.minted = broker.Minted{
				Kind: types.GrantAPIKey, JTI: "jti-host-pin",
				Injection: &egress.InjectionRule{
					Host: host, Header: "Authorization",
					SecretName: sentinel, Format: "Bearer %s",
				},
			}

			rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
			if rr.Code != http.StatusForbidden {
				t.Fatalf("%s -> host %q: status = %d, want 403; body=%s", sentinel, host, rr.Code, rr.Body.String())
			}
			if strings.Contains(rr.Body.String(), live) {
				t.Fatalf("%s -> host %q: the refusal body leaked the live token: %s", sentinel, host, rr.Body.String())
			}
			for _, ev := range h.audit.events {
				if ev.Action == "secret.read" && ev.Outcome == "success" {
					t.Fatalf("%s -> host %q: a successful secret.read was recorded for a refused injection", sentinel, host)
				}
			}
			if ev := lastAuditEvent(t, h.audit.events, "secret.read"); !strings.Contains(string(ev.Data), "oauth-host-not-anthropic") {
				t.Fatalf("%s -> host %q: audit data = %s, want the oauth-host-not-anthropic reason", sentinel, host, ev.Data)
			}
			// The token must not have been resolved (provider.Current rotates the
			// operator's own resident credentials) nor registered for masking.
			for _, v := range h.srv.cfg.MaskRegistry.Snapshot(runID) {
				if bytes.Contains(v, []byte(live)) {
					t.Fatalf("%s -> host %q: the live token was resolved and mask-registered despite the refusal", sentinel, host)
				}
			}
		}
	}
}

// TestValidateInlineSecretRefs_SentinelHostPin is the WRITE-TIME half: the same
// comparison in validateInlineSecretRefs, which rejects a mis-authored grant
// before it is ever stored.
func TestValidateInlineSecretRefs_SentinelHostPin(t *testing.T) {
	h, _ := newSecretsHarness(t)
	h.srv.cfg.SubscriptionToken = fakeSubToken{}
	h.srv.cfg.ManagedToken = fakeSubToken{}
	defer func() { h.srv.cfg.SubscriptionToken, h.srv.cfg.ManagedToken = nil, nil }()
	ctx := context.Background()

	sentinelAt := func(secretName, host string) types.RunPolicySpec {
		return types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			EligibleGrants: []types.GrantSpec{{
				Kind:  types.GrantAPIKey,
				Scope: mustJSON(map[string]any{"host": host, "secret_name": secretName}),
			}},
		}
	}

	for _, sentinel := range []string{types.SubscriptionOAuthSecret, types.ManagedOAuthSecret} {
		// The control: the pinned host still validates, so a failure below is the
		// host pin and not the sentinel path breaking wholesale.
		if code, err := h.srv.validateInlineSecretRefs(ctx, "", sentinelAt(sentinel, "api.anthropic.com")); err != nil || code != 0 {
			t.Fatalf("%s at api.anthropic.com: code=%d err=%v, want (0,nil)", sentinel, code, err)
		}
		for _, host := range []string{"evil.attacker.example", "api.anthropic.com.evil.example"} {
			code, err := h.srv.validateInlineSecretRefs(ctx, "", sentinelAt(sentinel, host))
			if err == nil || code != http.StatusUnprocessableEntity {
				t.Fatalf("%s at %q: code=%d err=%v, want (422,err) — a sentinel grant may only target %s",
					sentinel, host, code, err, subscriptionInjectionHost)
			}
			if !strings.Contains(err.Error(), subscriptionInjectionHost) {
				t.Fatalf("%s at %q: refusal %q should name the only permitted host", sentinel, host, err)
			}
		}
	}
}

// TestInternalInjection_SentinelSuccessForcesBearerAndMasks covers the sentinel
// SUCCESS path, which no test reached: every existing sentinel test takes a
// refusal branch, so the forced Authorization/Bearer clamp (which neutralises a
// crossed-wire RECORDED grant) and the MaskRegistry.Add of the live OAuth token
// were both uncovered.
func TestInternalInjection_SentinelSuccessForcesBearerAndMasks(t *testing.T) {
	const live = "oauth-live-token-value"
	exp := time.Now().Add(30 * time.Minute).UTC().Truncate(time.Millisecond)

	for _, sentinel := range []string{types.SubscriptionOAuthSecret, types.ManagedOAuthSecret} {
		h := sentinelHarness(t, liveOAuthProvider{value: live, expires: exp})
		runID := uuid.New()
		token := h.mintRunToken(t, runID)
		// A CROSSED-WIRE grant: the wrong header and a bare format, exactly what a
		// recorded profile can carry. The sink must clamp both.
		h.broker.minted = broker.Minted{
			Kind: types.GrantAPIKey, JTI: "jti-sentinel-ok",
			Injection: &egress.InjectionRule{
				Host: "api.anthropic.com", Header: "x-api-key",
				SecretName: sentinel, Format: "%s",
			},
		}

		rr := do(t, h.srv, http.MethodGet, "/api/v1/internal/injection/"+uuid.NewString(), token, "")
		if rr.Code != http.StatusOK {
			t.Fatalf("%s: status = %d, want 200; body=%s", sentinel, rr.Code, rr.Body.String())
		}
		var resp injectionResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%s: decode response: %v", sentinel, err)
		}
		if resp.Header != "Authorization" {
			t.Fatalf("%s: header = %q, want Authorization — the grant authored x-api-key and the sink must clamp it", sentinel, resp.Header)
		}
		if resp.Value != "Bearer "+live {
			t.Fatalf("%s: value = %q, want %q — the OAuth token has exactly one correct wire shape", sentinel, resp.Value, "Bearer "+live)
		}
		if resp.Host != "api.anthropic.com" {
			t.Fatalf("%s: host = %q, want api.anthropic.com", sentinel, resp.Host)
		}
		if resp.JTI != "jti-sentinel-ok" {
			t.Fatalf("%s: jti = %q, want the minted jti", sentinel, resp.JTI)
		}
		if resp.ExpiresAt != exp.UnixMilli() {
			t.Fatalf("%s: expires_at = %d, want %d (a provider expiry must be advertised)", sentinel, resp.ExpiresAt, exp.UnixMilli())
		}

		// BOTH forms are mask-registered: the raw token and the formatted header
		// value, since a PTY capture can carry either.
		snap := h.srv.cfg.MaskRegistry.Snapshot(runID)
		var rawSeen, formattedSeen bool
		for _, v := range snap {
			if bytes.Equal(v, []byte(live)) {
				rawSeen = true
			}
			if bytes.Equal(v, []byte("Bearer "+live)) {
				formattedSeen = true
			}
		}
		if !rawSeen || !formattedSeen {
			t.Fatalf("%s: mask registry holds raw=%v formatted=%v, want both (%d entries)", sentinel, rawSeen, formattedSeen, len(snap))
		}

		ev := lastAuditEvent(t, h.audit.events, "secret.read")
		if ev.Outcome != "success" {
			t.Fatalf("%s: last secret.read outcome = %q, want success", sentinel, ev.Outcome)
		}
		if !strings.Contains(string(ev.Data), "proxy-injection-subscription") || !strings.Contains(string(ev.Data), "jti-sentinel-ok") {
			t.Fatalf("%s: audit data = %s, want the injection purpose and the jti", sentinel, ev.Data)
		}
		if strings.Contains(string(ev.Data), live) {
			t.Fatalf("%s: the audit event carries the live token value: %s", sentinel, ev.Data)
		}
	}
}
