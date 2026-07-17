// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// getErrStore is a secretstore.Store whose Get returns a fixed error (or value),
// so readManagedBlob's error-classification branch can be exercised without a
// real backend.
type getErrStore struct {
	getErr error
	val    []byte
}

func (getErrStore) Name() string                                  { return "get-err" }
func (getErrStore) Put(context.Context, string, []byte) error     { return nil }
func (getErrStore) Delete(context.Context, string) error          { return nil }
func (getErrStore) List(context.Context) ([]string, error)        { return nil, nil }
func (s getErrStore) Get(context.Context, string) ([]byte, error) { return s.val, s.getErr }

// TestReadManagedBlob_DistinguishesStoreErrors pins U078: only ErrNotFound is
// "not connected" (found=false, err=nil). Any OTHER store error (decrypt failure
// after key rotation, backend down) MUST propagate rather than masquerade as
// "no credential connected". Reverting to a blanket `if err != nil { return
// false, nil }` makes the decrypt-failure case return a nil error and fails here.
func TestReadManagedBlob_DistinguishesStoreErrors(t *testing.T) {
	goodBlob, _ := json.Marshal(managedCredBlob{Token: "sk-ant-oat01-real-token"})
	tests := []struct {
		name    string
		store   secretstore.Store
		wantOK  bool
		wantErr bool
	}{
		{"absent is not-connected", getErrStore{getErr: secretstore.ErrNotFound}, false, false},
		{"wrapped absent is not-connected", getErrStore{getErr: fmt.Errorf("pg: %w", secretstore.ErrNotFound)}, false, false},
		{"decrypt failure propagates", getErrStore{getErr: errors.New("age: no identity matched key")}, false, true},
		{"backend down propagates", getErrStore{getErr: errors.New("dial tcp: connection refused")}, false, true},
		{"connected", getErrStore{val: goodBlob}, true, false},
		{"nil store is not-connected", nil, false, false},
	}
	h := newHarness(t)
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := h.srv.cfg
			cfg.Secrets = tc.store
			s := New(cfg)
			_, ok, err := s.readManagedBlob(context.Background(), "anthropic")
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (err %v)", ok, tc.wantOK, err)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
		})
	}
}

func TestManagedCredProvider(t *testing.T) {
	store := &memSecrets{m: map[string][]byte{}}
	p := NewManagedCredProvider(store, "anthropic")

	// Not connected: fails closed.
	if _, err := p.Current(context.Background()); err == nil {
		t.Fatal("expected error before a token is stored")
	}

	// Store a blob, then it resolves.
	blob, _ := json.Marshal(managedCredBlob{Token: "sk-ant-oat01-real-token"})
	_ = store.Put(context.Background(), harnessCredSecretName("anthropic"), blob)
	tok, err := p.Current(context.Background())
	if err != nil {
		t.Fatalf("Current after store: %v", err)
	}
	if tok.Value != "sk-ant-oat01-real-token" {
		t.Fatalf("wrong token: %q", tok.Value)
	}
	// Managed tokens carry no machine-readable expiry (zero) so the sink treats
	// them as static — no re-resolve churn.
	if !tok.ExpiresAt.IsZero() {
		t.Fatalf("managed token must have zero expiry, got %v", tok.ExpiresAt)
	}

	// Empty token blob == not connected.
	empty, _ := json.Marshal(managedCredBlob{Token: ""})
	_ = store.Put(context.Background(), harnessCredSecretName("anthropic"), empty)
	if _, err := p.Current(context.Background()); err == nil {
		t.Fatal("empty token must fail closed")
	}
}

func TestManagedSentinelCredsAreInert(t *testing.T) {
	raw, err := base64.StdEncoding.DecodeString(managedSentinelCredsB64())
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	var d struct {
		ClaudeAiOauth struct {
			AccessToken  string `json:"accessToken"`
			RefreshToken string `json:"refreshToken"`
			ExpiresAt    int64  `json:"expiresAt"`
		} `json:"claudeAiOauth"`
	}
	if uerr := json.Unmarshal(raw, &d); uerr != nil {
		t.Fatalf("unmarshal: %v", uerr)
	}
	if d.ClaudeAiOauth.AccessToken != managedSentinelAccessToken {
		t.Fatalf("access token is not the inert sentinel: %q", d.ClaudeAiOauth.AccessToken)
	}
	if d.ClaudeAiOauth.RefreshToken != "" {
		t.Fatal("sentinel must carry a BLANK refresh token")
	}
	if d.ClaudeAiOauth.ExpiresAt != 4102444800000 {
		t.Fatalf("sentinel expiry must be pinned far out, got %d", d.ClaudeAiOauth.ExpiresAt)
	}
}

func TestHarnessSecretIsReserved(t *testing.T) {
	// Every provider that supports container login must have its stored blob name
	// reserved, so the generic secrets API cannot clobber/list it and the injection
	// sink refuses to resolve it as a raw value. reservedSecret covers the
	// wardyn-harness-*-oauth PATTERN, so a future provider row is sealed
	// automatically — this test guards that the pattern actually matches every
	// name harnessCredSecretName generates.
	for _, agent := range []string{"claude-code"} {
		hl, ok := agentHarnessLogin(agent)
		if !ok {
			continue
		}
		if !reservedSecret(hl.secretName) {
			t.Fatalf("managed harness secret %q (provider %q) is NOT reserved — it would be listable/injectable as a raw value", hl.secretName, hl.provider)
		}
	}
	// The pattern must also seal a hypothetical future provider's blob.
	if !reservedSecret(harnessCredSecretName("codex")) {
		t.Fatal("reservedSecret must cover the wardyn-harness-<provider>-oauth pattern for future providers")
	}
}

func TestManagedOptOut_APIKeyInjectionWins(t *testing.T) {
	// The managed-subscription dispatch gate must stay a FALLBACK: when the run
	// already carries an anthropic api-key injection (the operator chose api-key),
	// managed must NOT fire and silently override it.
	anthropic := []runner.InjectionGrant{{Rule: egress.InjectionRule{Host: "api.anthropic.com"}}}
	if !hasAnthropicAPIKeyInjection(anthropic) {
		t.Fatal("should detect an api.anthropic.com injection")
	}
	// Trailing dot / case should still match (mirrors the sink host check).
	dotted := []runner.InjectionGrant{{Rule: egress.InjectionRule{Host: "API.Anthropic.com."}}}
	if !hasAnthropicAPIKeyInjection(dotted) {
		t.Fatal("host match must normalize case + trailing dot")
	}
	// A non-anthropic injection (e.g. OpenAI) must NOT block managed.
	other := []runner.InjectionGrant{{Rule: egress.InjectionRule{Host: "api.openai.com"}}}
	if hasAnthropicAPIKeyInjection(other) {
		t.Fatal("a non-anthropic injection must not count")
	}
	if hasAnthropicAPIKeyInjection(nil) {
		t.Fatal("no injections must not count")
	}
}

// TestHarnessCredentialPaste_RegistersGlobalMask pins that a pasted setup-token
// is handed to the mask registry PROCESS-GLOBALLY. Nothing else ever registers
// it: the login run mints no credential, so its per-run snapshot is empty by
// construction, and this handler is the only point in wardynd that ever sees the
// value. Without the AddGlobal the token would pass verbatim through every
// stream masker in the process.
func TestHarnessCredentialPaste_RegistersGlobalMask(t *testing.T) {
	const token = "sk-ant-oat01-live-long-lived-harness-token-value"

	reg := secretmask.NewRegistry()
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = reg
	h.srv = New(cfg)

	w := do(t, h.srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken,
		`{"token":"`+token+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("paste: code = %d, want 200 (body %s)", w.Code, w.Body.String())
	}

	// The value must now be masked out of ANY run's stream, including runs that
	// did not exist when it was captured.
	masked := secretmask.NewMasker(reg.Snapshot(uuid.New())).Mask([]byte("printed " + token + " here"))
	if bytes.Contains(masked, []byte(token)) {
		t.Fatalf("pasted setup-token is not globally mask-registered: %q", masked)
	}
	if !bytes.Contains(masked, []byte("<secret-hidden>")) {
		t.Fatalf("expected the placeholder in %q", masked)
	}
}

// TestHarnessCredentialPaste_NilMaskRegistry proves the paste path stays nil-safe
// (masking is optional wiring; it must not become a required dependency).
func TestHarnessCredentialPaste_NilMaskRegistry(t *testing.T) {
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.Secrets = &memSecrets{m: map[string][]byte{}}
	cfg.MaskRegistry = nil
	h.srv = New(cfg)

	w := do(t, h.srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken,
		`{"token":"sk-ant-oat01-token-with-no-registry-wired"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("paste with nil MaskRegistry: code = %d, want 200", w.Code)
	}
}

// ── HTTP-router-level tests (through the real mux + humanOrAdminAuth) ─────────

// harnessCredSrv builds a Server with the harness login/credential routes MOUNTED
// (they mount only when cfg.Secrets != nil) over the given secret store, reusing
// the harness's embedded identity + audit recorder so audit assertions work.
func harnessCredSrv(t *testing.T, sec secretstore.Store) (*harness, *Server) {
	t.Helper()
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.Secrets = sec
	srv := New(cfg)
	h.srv = srv
	return h, srv
}

func auditHas(events []types.AuditEvent, action string) bool {
	for _, e := range events {
		if e.Action == action {
			return true
		}
	}
	return false
}

// failSecrets wraps memSecrets to force Put/Delete failures so the paste/disconnect
// handlers' store-error → 500 mapping is exercisable through the router.
type failSecrets struct {
	*memSecrets
	putErr, delErr error
}

func (s failSecrets) Put(ctx context.Context, n string, v []byte) error {
	if s.putErr != nil {
		return s.putErr
	}
	return s.memSecrets.Put(ctx, n, v)
}
func (s failSecrets) Delete(ctx context.Context, n string) error {
	if s.delErr != nil {
		return s.delErr
	}
	return s.memSecrets.Delete(ctx, n)
}

// TestHarnessRoutes_AuthRequired: every harness setup route sits in the
// humanOrAdminAuth group — an unauthenticated or wrong-token request must 401
// before any handler logic runs (capability disclosure / shared-credential
// mutation must never be anonymous).
func TestHarnessRoutes_AuthRequired(t *testing.T) {
	_, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}})
	routes := []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/setup/harness-login"},
		{http.MethodPut, "/api/v1/setup/harness-credential/anthropic"},
		{http.MethodDelete, "/api/v1/setup/harness-credential/anthropic"},
	}
	for _, rt := range routes {
		if w := do(t, srv, rt.method, rt.path, "", `{}`); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s no token: code = %d, want 401", rt.method, rt.path, w.Code)
		}
		if w := do(t, srv, rt.method, rt.path, "wrong-token", `{}`); w.Code != http.StatusUnauthorized {
			t.Errorf("%s %s wrong token: code = %d, want 401", rt.method, rt.path, w.Code)
		}
	}
}

// TestHarnessRoutes_MethodDiscipline: the registered paths reject unregistered
// verbs with 405 (chi's method-not-allowed), never silently accepting them.
func TestHarnessRoutes_MethodDiscipline(t *testing.T) {
	_, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}})
	cases := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/setup/harness-login"},                 // only POST
		{http.MethodPut, "/api/v1/setup/harness-login"},                 // only POST
		{http.MethodGet, "/api/v1/setup/harness-credential/anthropic"},  // only PUT/DELETE
		{http.MethodPost, "/api/v1/setup/harness-credential/anthropic"}, // only PUT/DELETE
	}
	for _, c := range cases {
		if w := do(t, srv, c.method, c.path, adminToken, `{}`); w.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s %s: code = %d, want 405", c.method, c.path, w.Code)
		}
	}
}

// TestHandleHarnessLogin_ErrorMapping covers the router-reachable error paths of
// the login launcher short of a full sandbox dispatch (which needs a runner +
// run store; the launch machinery itself is covered by the dispatch/interactive
// tests). Bad body → 400, unsupported provider → 400, launch failure → 500.
func TestHandleHarnessLogin_ErrorMapping(t *testing.T) {
	_, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}})
	const path = "/api/v1/setup/harness-login"

	// Malformed JSON body → 400.
	if w := do(t, srv, http.MethodPost, path, adminToken, `{`); w.Code != http.StatusBadRequest {
		t.Errorf("bad body: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	// A provider with no container-login convention → 400.
	if w := do(t, srv, http.MethodPost, path, adminToken, `{"provider":"openai"}`); w.Code != http.StatusBadRequest {
		t.Errorf("unknown provider: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	// Valid provider (default anthropic) but no runner configured → the launch
	// fails and maps to 500 (harness has no Runner).
	if w := do(t, srv, http.MethodPost, path, adminToken, `{}`); w.Code != http.StatusInternalServerError {
		t.Errorf("no-runner launch: code = %d, want 500; body=%s", w.Code, w.Body.String())
	}
}

// TestHandleHarnessCredentialPaste_HappyPath: a well-formed setup-token is stored
// under the RESERVED name, the response reports captured:true, and a
// harness.credential.captured audit event is written.
func TestHandleHarnessCredentialPaste_HappyPath(t *testing.T) {
	const token = "sk-ant-oat01-happy-path-stored-token"
	sec := &memSecrets{m: map[string][]byte{}}
	h, srv := harnessCredSrv(t, sec)

	w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken,
		`{"token":"`+token+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("paste: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp["captured"] != true || resp["provider"] != "anthropic" {
		t.Errorf("resp = %v, want captured:true provider:anthropic", resp)
	}
	raw, ok := sec.m[harnessCredSecretName("anthropic")]
	if !ok {
		t.Fatal("token blob was not stored under the reserved harness name")
	}
	var blob managedCredBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("stored blob is not a managedCredBlob: %v", err)
	}
	if blob.Token != token {
		t.Errorf("stored token = %q, want %q", blob.Token, token)
	}
	if !auditHas(h.audit.events, "harness.credential.captured") {
		t.Error("no harness.credential.captured audit event")
	}
}

// TestHandleHarnessCredentialPaste_Errors covers the handler's 4xx/5xx mapping:
// unknown provider, malformed body, wrong token prefix, and a store Put failure.
func TestHandleHarnessCredentialPaste_Errors(t *testing.T) {
	// Unknown provider → 400 (checked before the body is read).
	if _, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}}); true {
		if w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/openai", adminToken,
			`{"token":"sk-ant-oat01-x"}`); w.Code != http.StatusBadRequest {
			t.Errorf("unknown provider: code = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	}
	// Malformed JSON → 400.
	if _, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}}); true {
		if w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken, `{`); w.Code != http.StatusBadRequest {
			t.Errorf("bad body: code = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	}
	// A token that fails the format guard (wrong prefix) → 400, and nothing stored.
	if _, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}}); true {
		if w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken,
			`{"token":"not-a-setup-token"}`); w.Code != http.StatusBadRequest {
			t.Errorf("bad prefix: code = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	}
	// A store Put failure maps to 500 (not swallowed as success).
	fail := failSecrets{memSecrets: &memSecrets{m: map[string][]byte{}}, putErr: errors.New("age: no identity matched key")}
	if _, srv := harnessCredSrv(t, fail); true {
		if w := do(t, srv, http.MethodPut, "/api/v1/setup/harness-credential/anthropic", adminToken,
			`{"token":"sk-ant-oat01-store-will-fail"}`); w.Code != http.StatusInternalServerError {
			t.Errorf("store Put failure: code = %d, want 500; body=%s", w.Code, w.Body.String())
		}
	}
}

// TestHandleHarnessDisconnect_HappyPath: DELETE removes the stored blob, reports
// captured:false, and writes a harness.credential.disconnected audit event.
func TestHandleHarnessDisconnect_HappyPath(t *testing.T) {
	sec := &memSecrets{m: map[string][]byte{
		harnessCredSecretName("anthropic"): []byte(`{"token":"sk-ant-oat01-existing"}`),
	}}
	h, srv := harnessCredSrv(t, sec)

	w := do(t, srv, http.MethodDelete, "/api/v1/setup/harness-credential/anthropic", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("disconnect: code = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	var resp map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode resp: %v", err)
	}
	if resp["captured"] != false || resp["provider"] != "anthropic" {
		t.Errorf("resp = %v, want captured:false provider:anthropic", resp)
	}
	if _, ok := sec.m[harnessCredSecretName("anthropic")]; ok {
		t.Error("stored blob was not deleted")
	}
	if !auditHas(h.audit.events, "harness.credential.disconnected") {
		t.Error("no harness.credential.disconnected audit event")
	}
}

// TestHandleHarnessDisconnect_Errors: unknown provider → 400, store Delete
// failure → 500.
func TestHandleHarnessDisconnect_Errors(t *testing.T) {
	if _, srv := harnessCredSrv(t, &memSecrets{m: map[string][]byte{}}); true {
		if w := do(t, srv, http.MethodDelete, "/api/v1/setup/harness-credential/openai", adminToken, ""); w.Code != http.StatusBadRequest {
			t.Errorf("unknown provider: code = %d, want 400; body=%s", w.Code, w.Body.String())
		}
	}
	fail := failSecrets{memSecrets: &memSecrets{m: map[string][]byte{}}, delErr: errors.New("pg: connection refused")}
	if _, srv := harnessCredSrv(t, fail); true {
		if w := do(t, srv, http.MethodDelete, "/api/v1/setup/harness-credential/anthropic", adminToken, ""); w.Code != http.StatusInternalServerError {
			t.Errorf("store Delete failure: code = %d, want 500; body=%s", w.Code, w.Body.String())
		}
	}
}

func TestAgentHarnessLogin(t *testing.T) {
	hl, ok := agentHarnessLogin("claude-code")
	if !ok {
		t.Fatal("claude-code must support container login")
	}
	if hl.sentinel != types.ManagedOAuthSecret {
		t.Fatalf("wrong sentinel: %q", hl.sentinel)
	}
	if hl.injectHost != "api.anthropic.com" {
		t.Fatalf("wrong inject host pin: %q", hl.injectHost)
	}
	// Codex has no container-login path in v1.
	if _, ok := agentHarnessLogin("codex-cli"); ok {
		t.Fatal("codex-cli must NOT support container login in v1")
	}
}
