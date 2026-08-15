// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ssoLoginRunStore is a minimal store.Store returning a fixed run from
// GetRun — the only method the sso-token upload handler needs.
type ssoLoginRunStore struct {
	store.Store
	run types.AgentRun
}

func (s ssoLoginRunStore) GetRun(context.Context, uuid.UUID) (types.AgentRun, error) {
	return s.run, nil
}

// newSSOUploadSrv wires a Server over an aws-sso harness-login run + an
// in-memory secret store, and returns a valid run token for that run.
func newSSOUploadSrv(t *testing.T) (*Server, *memSecrets, string, uuid.UUID) {
	t.Helper()
	h := newHarness(t)
	runID := uuid.New()
	st := ssoLoginRunStore{run: types.AgentRun{ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent}}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	srv := New(cfg)
	return srv, sec, h.mintRunToken(t, runID), runID
}

const validSSOBody = `{
	"access_token": "aws-sso-access-token-value",
	"refresh_token": "aws-sso-refresh-token-value",
	"client_id": "client-id",
	"client_secret": "client-secret",
	"start_url": "https://my-sso.awsapps.com/start",
	"region": "us-west-2",
	"account_id": "123456789012",
	"role_name": "WardynBedrockRole",
	"expires_at": "2100-01-01T00:00:00Z"
}`

// TestUploadSSOToken_HappyPath: a well-formed SSO token blob is stored under
// the reserved aws harness secret with server-stamped provenance, and a
// harness.credential.captured audit event is written.
func TestUploadSSOToken_HappyPath(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	st := ssoLoginRunStore{run: types.AgentRun{ID: runID, Task: harnessLoginTask, Agent: awsSSOAgent}}
	sec := &memSecrets{m: map[string][]byte{}}
	cfg := baseTestConfig(h, st)
	cfg.Secrets = sec
	srv := New(cfg)
	h.srv = srv
	tok := h.mintRunToken(t, runID)

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
	if w.Code != http.StatusNoContent {
		t.Fatalf("upload: code = %d, want 204; body=%s", w.Code, w.Body.String())
	}

	raw, ok := sec.m[harnessCredSecretName(awsSSOProvider)]
	if !ok {
		t.Fatal("sso token blob was not stored under the reserved harness name")
	}
	var blob awsSSOBlob
	if err := json.Unmarshal(raw, &blob); err != nil {
		t.Fatalf("stored blob is not an awsSSOBlob: %v", err)
	}
	if blob.AccessToken != "aws-sso-access-token-value" {
		t.Errorf("stored access token = %q", blob.AccessToken)
	}
	if blob.SourceRunID != runID.String() {
		t.Errorf("SourceRunID = %q, want %q (server-stamped)", blob.SourceRunID, runID.String())
	}
	if blob.CapturedAt.IsZero() {
		t.Error("CapturedAt was not server-stamped")
	}
	if !auditHas(h.audit.events, "harness.credential.captured") {
		t.Error("no harness.credential.captured audit event")
	}
}

// TestUploadSSOToken_InvalidBlobRejected: a structurally incomplete blob
// (missing start_url) is rejected 400 and nothing is stored — this is the
// AWS-side replacement for the Anthropic prefix guard.
func TestUploadSSOToken_InvalidBlobRejected(t *testing.T) {
	srv, sec, tok, runID := newSSOUploadSrv(t)
	incomplete := `{"access_token":"tok-only","region":"us-west-2","expires_at":"2100-01-01T00:00:00Z"}`

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, incomplete)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("incomplete blob: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("an invalid blob must not be stored")
	}
}

// TestUploadSSOToken_HalfResolvedCaptureRejected is the W5-S1-4 regression:
// wardyn-aws-sso's account/role resolution (`aws sso list-accounts` /
// list-account-roles) is best-effort and can come up empty (no accounts, a
// timeout, a malformed response) while every OTHER field is well-formed. Before
// the fix, awsSSOBlob.valid() didn't require account_id/role_name, so this
// half-resolved capture was accepted and stored — and resolveBedrockAuth
// selects a stored SSO credential ahead of the host-mode ~/.aws mount and
// static-key lanes, so a capture that can never satisfy GetRoleCredentials
// would silently pre-empt lanes that might have actually worked.
func TestUploadSSOToken_HalfResolvedCaptureRejected(t *testing.T) {
	srv, sec, tok, runID := newSSOUploadSrv(t)
	halfResolved := `{
		"access_token": "aws-sso-access-token-value",
		"start_url": "https://my-sso.awsapps.com/start",
		"region": "us-west-2",
		"expires_at": "2100-01-01T00:00:00Z"
	}`

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, halfResolved)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("half-resolved capture (no account_id/role_name): code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("a half-resolved capture must not be stored — it would pre-empt a working Bedrock lane")
	}
}

// TestUploadSSOToken_MaliciousStartURLRejected is a W15-d defense-in-depth
// regression: before the fix, only blob.valid()'s non-empty check ran on
// start_url, so a newline-bearing value would later be baked VERBATIM, with no
// escaping, into every subsequent Bedrock run's ~/.aws/config INI
// (awsSSOConfigFileContents, runs_bedrock.go's fmt.Sprintf) — smuggling extra
// INI keys/sections into a file shared across every run that credential mode
// serves, since the blob is captured ONCE and reused thereafter. start_url now
// takes the same https-URL/no-whitespace guard the operator's own pre-login
// input already takes (validateSSOStartURL, harnesscred.go).
func TestUploadSSOToken_MaliciousStartURLRejected(t *testing.T) {
	srv, sec, tok, runID := newSSOUploadSrv(t)
	malicious := `{
		"access_token": "aws-sso-access-token-value",
		"start_url": "https://my-sso.awsapps.com/start\n[profile evil]\nregion=us-east-1",
		"region": "us-west-2",
		"expires_at": "2100-01-01T00:00:00Z"
	}`

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, malicious)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("newline in start_url: code = %d, want 400; body=%s", w.Code, w.Body.String())
	}
	if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
		t.Error("a blob with a control character in start_url must not be stored")
	}
}

// TestUploadSSOToken_ControlCharsInAccountOrRoleRejected is the other half of
// the same W15-d defense-in-depth guard: sso_account_id, sso_role_name, and
// sso_region ride the identical unescaped INI template near start_url
// (awsSSOConfigFileContents), so a newline in any is exactly as dangerous
// and must be rejected the same way (repoFieldSafe — the same control-
// character guard run.Repo already takes, for the identical reason). The
// "region" case is the load-bearing one: sso_region is written AFTER
// sso_start_url, so an injected duplicate sso_start_url via region would win
// under last-key-wins parsing and silently defeat the StartURL guard.
func TestUploadSSOToken_ControlCharsInAccountOrRoleRejected(t *testing.T) {
	cases := map[string]string{
		"account_id": `{"access_token":"tok","start_url":"https://my-sso.awsapps.com/start","region":"us-west-2","expires_at":"2100-01-01T00:00:00Z","account_id":"123456789012\n[profile evil]"}`,
		"role_name":  `{"access_token":"tok","start_url":"https://my-sso.awsapps.com/start","region":"us-west-2","expires_at":"2100-01-01T00:00:00Z","role_name":"AdminRole\n[profile evil]"}`,
		"region":     `{"access_token":"tok","start_url":"https://my-sso.awsapps.com/start","region":"us-west-2\nsso_start_url = https://attacker.example.com/start","expires_at":"2100-01-01T00:00:00Z"}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv, sec, tok, runID := newSSOUploadSrv(t)
			w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, body)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("control char in %s: code = %d, want 400; body=%s", name, w.Code, w.Body.String())
			}
			if _, ok := sec.m[harnessCredSecretName(awsSSOProvider)]; ok {
				t.Errorf("a blob with a control character in %s must not be stored", name)
			}
		})
	}
}

// TestUploadSSOToken_NonHarnessLoginRunRejected: the run-kind check is TRUSTED
// server state (run.Task/run.Agent), not sandbox input. An ordinary run (or a
// harness-login run for a different provider) has no business posting an AWS
// SSO credential.
func TestUploadSSOToken_NonHarnessLoginRunRejected(t *testing.T) {
	h := newHarness(t)
	runID := uuid.New()
	tok := h.mintRunToken(t, runID)

	cases := []types.AgentRun{
		{ID: runID, Task: "some other task", Agent: awsSSOAgent},
		{ID: runID, Task: harnessLoginTask, Agent: "claude-code"}, // right task, wrong agent/provider
		{ID: runID},
	}
	for _, run := range cases {
		st := ssoLoginRunStore{run: run}
		cfg := baseTestConfig(h, st)
		cfg.Secrets = &memSecrets{m: map[string][]byte{}}
		srv := New(cfg)

		w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+runID.String(), tok, validSSOBody)
		if w.Code != http.StatusForbidden {
			t.Errorf("run %+v: code = %d, want 403; body=%s", run, w.Code, w.Body.String())
		}
	}
}

// TestUploadSSOToken_CrossRunRejected mirrors the scan/verify cross-run
// guard: a run token minted for run A must not be able to PUT an sso token
// under run B's id, before any store lookup or body parse.
func TestUploadSSOToken_CrossRunRejected(t *testing.T) {
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.Secrets = &memSecrets{m: map[string][]byte{}} // route mounts only when Secrets != nil
	srv := New(cfg)
	tok := h.mintRunToken(t, uuid.New())
	otherRun := uuid.New()

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+otherRun.String(), tok, validSSOBody)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-run sso-token upload: code = %d, want 403; body=%s", w.Code, w.Body.String())
	}
}

// TestUploadSSOToken_NoSecretStoreRejected: with no secret store configured
// the handler must fail closed (503), never silently accept and drop the
// credential. Invoked directly (the route itself is conditionally mounted on
// cfg.Secrets != nil, mirroring the harness-login/paste routes — see
// server.go — so this pins the handler's own defensive guard).
func TestUploadSSOToken_NoSecretStoreRejected(t *testing.T) {
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.Secrets = nil
	srv := New(cfg)

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPut, "/api/v1/internal/sso-token/"+uuid.New().String(),
		nil)
	srv.handleUploadSSOToken(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret store: code = %d, want 503; body=%s", w.Code, w.Body.String())
	}
}

// TestUploadSSOToken_RouteNotMountedWithoutSecrets pins that the route itself
// is absent (404) when no secret store is configured, matching the
// harness-login/paste route convention.
func TestUploadSSOToken_RouteNotMountedWithoutSecrets(t *testing.T) {
	h := newHarness(t)
	cfg := h.srv.cfg
	cfg.Secrets = nil
	srv := New(cfg)
	tok := h.mintRunToken(t, uuid.New())

	w := do(t, srv, http.MethodPut, "/api/v1/internal/sso-token/"+uuid.New().String(), tok, validSSOBody)
	if w.Code != http.StatusNotFound {
		t.Fatalf("route without secrets: code = %d, want 404; body=%s", w.Code, w.Body.String())
	}
}
