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
