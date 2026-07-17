// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// testToken is used by every test server and client pair.
const testToken = "test-admin-token"

// newTestClient creates a Client wired to the given httptest server.
func newTestClient(srv *httptest.Server) *client.Client {
	return &client.Client{
		BaseURL:    srv.URL,
		Token:      testToken,
		HTTPClient: srv.Client(),
	}
}

// writeJSON is a minimal test helper that sets headers and encodes v.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// --------------------------------------------------------------------------
// CreateRun
// --------------------------------------------------------------------------

func TestCreateRun_Success(t *testing.T) {
	want := types.AgentRun{
		ID:               uuid.New(),
		CreatedAt:        time.Now().UTC().Truncate(time.Second),
		UpdatedAt:        time.Now().UTC().Truncate(time.Second),
		CreatedBy:        "admin",
		Agent:            "claude-code",
		Repo:             "org/repo",
		Task:             "fix bug",
		ConfinementClass: types.CC2,
		State:            types.RunPending,
		SPIFFEID:         "spiffe://example/agent-run/x",
		RunnerTarget:     "docker",
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runs" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["agent"] != "claude-code" || body["repo"] != "org/repo" {
			t.Errorf("unexpected body: %v", body)
		}
		writeJSON(w, http.StatusCreated, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).CreateRun(context.Background(), client.CreateRunRequest{
		Agent: "claude-code",
		Repo:  "org/repo",
		Task:  "fix bug",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("got ID %v, want %v", got.ID, want.ID)
	}
	if got.Agent != want.Agent {
		t.Errorf("got Agent %q, want %q", got.Agent, want.Agent)
	}
}

// --------------------------------------------------------------------------
// GetRun
// --------------------------------------------------------------------------

func TestGetRun_Success(t *testing.T) {
	id := uuid.New()
	want := types.AgentRun{ID: id, Agent: "codex-cli", State: types.RunRunning}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runs/"+id.String() {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).GetRun(context.Background(), id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != id {
		t.Errorf("got ID %v, want %v", got.ID, id)
	}
}

// --------------------------------------------------------------------------
// ListRuns
// --------------------------------------------------------------------------

func TestListRuns_Success(t *testing.T) {
	runs := []types.AgentRun{
		{ID: uuid.New(), Agent: "claude-code"},
		{ID: uuid.New(), Agent: "codex-cli"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runs" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, runs)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListRuns(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d runs, want 2", len(got))
	}
}

func TestListRuns_Empty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []types.AgentRun{})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListRuns(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d runs, want 0", len(got))
	}
}

// --------------------------------------------------------------------------
// KillRun
// --------------------------------------------------------------------------

func TestKillRun_Success(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/runs/"+id.String()+"/kill" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusAccepted, map[string]any{"id": id, "state": types.RunKilled})
	}))
	defer srv.Close()

	resp, err := newTestClient(srv).KillRun(context.Background(), id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if resp.State != types.RunKilled {
		t.Errorf("got state %q, want KILLED", resp.State)
	}
}

// --------------------------------------------------------------------------
// ListApprovals
// --------------------------------------------------------------------------

func TestListApprovals_AllStates(t *testing.T) {
	approvals := []types.ApprovalRequest{
		{ID: uuid.New(), State: types.ApprovalPending},
		{ID: uuid.New(), State: types.ApprovalApproved},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("state") != "" {
			t.Errorf("expected no state filter, got %q", r.URL.Query().Get("state"))
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, approvals)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListApprovals(context.Background(), "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d approvals, want 2", len(got))
	}
}

func TestListApprovals_StateFilter(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("state"); got != string(types.ApprovalPending) {
			t.Errorf("got state %q, want PENDING", got)
		}
		writeJSON(w, http.StatusOK, []types.ApprovalRequest{})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).ListApprovals(context.Background(), types.ApprovalPending)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --------------------------------------------------------------------------
// Approve
// --------------------------------------------------------------------------

func TestApprove_Success(t *testing.T) {
	id := uuid.New()
	want := types.ApprovalRequest{ID: id, State: types.ApprovalApproved, Reason: "looks fine"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/approvals/"+id.String()+"/approve" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["reason"] != "looks fine" {
			t.Errorf("expected reason 'looks fine', got %q", body["reason"])
		}
		writeJSON(w, http.StatusOK, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).Approve(context.Background(), id, "looks fine")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.State != types.ApprovalApproved {
		t.Errorf("got state %q, want APPROVED", got.State)
	}
}

// --------------------------------------------------------------------------
// Deny
// --------------------------------------------------------------------------

func TestDeny_Success(t *testing.T) {
	id := uuid.New()
	want := types.ApprovalRequest{ID: id, State: types.ApprovalDenied, Reason: "not safe"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/approvals/"+id.String()+"/deny" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).Deny(context.Background(), id, "not safe")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.State != types.ApprovalDenied {
		t.Errorf("got state %q, want DENIED", got.State)
	}
}

// --------------------------------------------------------------------------
// AuditEvents
// --------------------------------------------------------------------------

func TestAuditEvents_Success(t *testing.T) {
	runID := uuid.New()
	events := []types.AuditEvent{
		{ID: uuid.New(), Action: "run.create", Outcome: "success"},
		{ID: uuid.New(), Action: "run.kill", Outcome: "success"},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/audit" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if got := r.URL.Query().Get("run_id"); got != runID.String() {
			t.Errorf("got run_id %q, want %q", got, runID.String())
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, events)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).AuditEvents(context.Background(), runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Errorf("got %d events, want 2", len(got))
	}
	if got[0].Action != "run.create" {
		t.Errorf("got action %q, want run.create", got[0].Action)
	}
}

// --------------------------------------------------------------------------
// Auth and Principal header
// --------------------------------------------------------------------------

func TestMissingToken_Returns401(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "missing bearer token"})
	}))
	defer srv.Close()

	c := &client.Client{BaseURL: srv.URL, Token: "", HTTPClient: srv.Client()}
	_, err := c.ListRuns(context.Background())
	assertAPIError(t, err, http.StatusUnauthorized)
}

func TestPrincipalHeader_IsSent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-Wardyn-Principal"); got != "alice" {
			t.Errorf("got X-Wardyn-Principal %q, want alice", got)
		}
		writeJSON(w, http.StatusOK, []types.AgentRun{})
	}))
	defer srv.Close()

	c := &client.Client{
		BaseURL:    srv.URL,
		Token:      testToken,
		HTTPClient: srv.Client(),
		Principal:  "alice",
	}
	_, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --------------------------------------------------------------------------
// Error type helpers
// --------------------------------------------------------------------------

func TestAPIError_ErrorString(t *testing.T) {
	err := &client.APIError{Status: 404, Body: `{"error":"not found"}`}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("error string does not contain status: %s", err.Error())
	}
	// The SDK is a public library and must NOT hardcode the consuming CLI's
	// name: only cmd/wardyn owns the "wardyn:" prefix. Leaking it here is what
	// produced the doubled "wardyn: wardyn:" line the review caught.
	if strings.Contains(err.Error(), "wardyn:") {
		t.Errorf("SDK error must not carry a 'wardyn:' prefix: %s", err.Error())
	}
}

func TestAPIError_ErrorsAs(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "policy error"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).ListRuns(context.Background())
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("errors.As did not find *APIError: %T %v", err, err)
	}
	if apiErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("got Status %d, want %d", apiErr.Status, http.StatusUnprocessableEntity)
	}
}

// --------------------------------------------------------------------------
// BaseURL trailing-slash tolerance
// --------------------------------------------------------------------------

func TestBaseURL_TrailingSlash(t *testing.T) {
	runs := []types.AgentRun{{ID: uuid.New(), Agent: "claude-code"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, runs)
	}))
	defer srv.Close()

	c := &client.Client{
		BaseURL:    srv.URL + "/",
		Token:      testToken,
		HTTPClient: srv.Client(),
	}
	got, err := c.ListRuns(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("got %d runs, want 1", len(got))
	}
}

// --------------------------------------------------------------------------
// Secrets: ListSecrets / SetSecret / DeleteSecret
// --------------------------------------------------------------------------

func TestListSecrets_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/secrets" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, map[string]any{"names": []string{"anthropic-api-key", "openai-key"}})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListSecrets(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0] != "anthropic-api-key" || got[1] != "openai-key" {
		t.Errorf("got names %v, want [anthropic-api-key openai-key]", got)
	}
}

func TestSetSecret_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/secrets/anthropic-api-key" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["value"] != "sk-ant-123" {
			t.Errorf("got value %q, want sk-ant-123", body["value"])
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv).SetSecret(context.Background(), "anthropic-api-key", "sk-ant-123"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestSetSecret_NameEscaped verifies the secret name is path-escaped (a name
// with a slash must not break the path layout — it is percent-encoded).
func TestSetSecret_NameEscaped(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// url.PathEscape encodes "/" as %2F; the raw request path carries it
		// encoded so it cannot be read as a path separator.
		if got := r.URL.EscapedPath(); got != "/api/v1/secrets/a%2Fb" {
			t.Errorf("got escaped path %q, want /api/v1/secrets/a%%2Fb", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv).SetSecret(context.Background(), "a/b", "v"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeleteSecret_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/secrets/old-key" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv).DeleteSecret(context.Background(), "old-key"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --------------------------------------------------------------------------
// CreateRun with inline_policy (serialization contract)
// --------------------------------------------------------------------------

// TestCreateRun_InlinePolicySerialized asserts the SDK serializes the
// inline_policy field (a *RunPolicySpec) on the wire under the exact JSON key,
// carrying its nested nouns (workspace_mounts, eligible_grants).
func TestCreateRun_InlinePolicySerialized(t *testing.T) {
	want := types.AgentRun{ID: uuid.New(), Agent: "claude-code", State: types.RunPending}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if _, ok := body["policy_id"]; ok {
			t.Errorf("policy_id must be omitted when only inline_policy is set: %v", body)
		}
		ip, ok := body["inline_policy"].(map[string]any)
		if !ok {
			t.Fatalf("inline_policy missing or wrong type: %v", body["inline_policy"])
		}
		if ip["min_confinement_class"] != "CC2" {
			t.Errorf("inline_policy.min_confinement_class = %v, want CC2", ip["min_confinement_class"])
		}
		mounts, ok := ip["workspace_mounts"].([]any)
		if !ok || len(mounts) != 1 {
			t.Fatalf("inline_policy.workspace_mounts = %v, want 1 entry", ip["workspace_mounts"])
		}
		m0 := mounts[0].(map[string]any)
		if m0["target"] != "/work" || m0["source"] != "/home/me/project" {
			t.Errorf("mount = %v, want source=/home/me/project target=/work", m0)
		}
		writeJSON(w, http.StatusCreated, want)
	}))
	defer srv.Close()

	rw := false
	req := client.CreateRunRequest{
		Agent: "claude-code",
		InlinePolicy: &client.RunPolicySpec{
			MinConfinementClass: client.CC2,
			WorkspaceMounts: []client.WorkspaceMount{
				{Source: "/home/me/project", Target: "/work", ReadOnly: &rw},
			},
		},
	}
	got, err := newTestClient(srv).CreateRun(context.Background(), req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("got ID %v, want %v", got.ID, want.ID)
	}
}

// TestCreateRun_NoInlinePolicyOmitted asserts inline_policy is omitted from the
// wire when unset (omitempty on a nil pointer), so existing callers are
// unaffected and the default-policy path is preserved.
func TestCreateRun_NoInlinePolicyOmitted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if _, ok := body["inline_policy"]; ok {
			t.Errorf("inline_policy must be omitted when nil: %v", body)
		}
		writeJSON(w, http.StatusCreated, types.AgentRun{ID: uuid.New()})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).CreateRun(context.Background(), client.CreateRunRequest{
		Agent: "claude-code", Repo: "org/repo",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

// --------------------------------------------------------------------------
// Large success body (HIGH: 2 KiB cap must NOT truncate success responses)
// --------------------------------------------------------------------------

// TestSuccessBody_LargerThan2KiB_DecodesFully is a regression test for the
// finding that do() applied a 2048-byte LimitReader to ALL bodies and then
// json.Unmarshal'd that capped buffer on the 2xx path — so any success
// response larger than 2 KiB failed to decode. The 2 KiB cap must apply ONLY
// to error (non-2xx) bodies; success bodies must decode in full.
func TestSuccessBody_LargerThan2KiB_DecodesFully(t *testing.T) {
	// Build a run whose JSON serialization comfortably exceeds 2048 bytes by
	// stuffing the Task field with a long string. uuid + fixed fields plus a
	// >3 KiB task guarantees a body well past the old cap.
	bigTask := strings.Repeat("x", 3000)
	want := types.AgentRun{
		ID:    uuid.New(),
		Agent: "claude-code",
		Repo:  "org/repo",
		Task:  bigTask,
		State: types.RunPending,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		// Sanity-check the encoded body is actually larger than the old cap,
		// so this test genuinely exercises the truncation path.
		raw, _ := json.Marshal(want)
		if len(raw) <= 2048 {
			t.Fatalf("test body is %d bytes; must exceed 2048 to exercise the cap", len(raw))
		}
		writeJSON(w, http.StatusOK, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).GetRun(context.Background(), want.ID)
	if err != nil {
		t.Fatalf("unexpected error decoding large success body: %v", err)
	}
	if got.ID != want.ID {
		t.Errorf("got ID %v, want %v", got.ID, want.ID)
	}
	if got.Task != bigTask {
		t.Errorf("Task truncated: got %d bytes, want %d", len(got.Task), len(bigTask))
	}
}

// --------------------------------------------------------------------------
// Error paths (table-driven): each entry serves one error status from a
// shared server and asserts the call surfaces it as a *client.APIError.
// --------------------------------------------------------------------------

func TestAPIErrorPaths(t *testing.T) {
	id := uuid.New()
	tests := []struct {
		name   string
		status int
		call   func(*client.Client) error
	}{
		{"CreateRun_APIError", http.StatusBadRequest, func(c *client.Client) error {
			_, err := c.CreateRun(context.Background(), client.CreateRunRequest{})
			return err
		}},
		{"GetRun_NotFound", http.StatusNotFound, func(c *client.Client) error {
			_, err := c.GetRun(context.Background(), id)
			return err
		}},
		{"KillRun_NotFound", http.StatusNotFound, func(c *client.Client) error {
			_, err := c.KillRun(context.Background(), id)
			return err
		}},
		{"Approve_AlreadyDecided", http.StatusConflict, func(c *client.Client) error {
			_, err := c.Approve(context.Background(), id, "")
			return err
		}},
		{"Deny_NotFound", http.StatusNotFound, func(c *client.Client) error {
			_, err := c.Deny(context.Background(), id, "")
			return err
		}},
		{"AuditEvents_BadRequest", http.StatusBadRequest, func(c *client.Client) error {
			_, err := c.AuditEvents(context.Background(), uuid.Nil)
			return err
		}},
		{"ListSecrets_APIError", http.StatusInternalServerError, func(c *client.Client) error {
			_, err := c.ListSecrets(context.Background())
			return err
		}},
		{"SetSecret_Reserved403", http.StatusForbidden, func(c *client.Client) error {
			return c.SetSecret(context.Background(), "wardyn-signing-key", "x")
		}},
		{"DeleteSecret_NotFound", http.StatusInternalServerError, func(c *client.Client) error {
			return c.DeleteSecret(context.Background(), "missing")
		}},
		{"ListGrants_NotFound", http.StatusNotFound, func(c *client.Client) error {
			_, err := c.ListGrants(context.Background(), id)
			return err
		}},
		{"GetPolicy_NotFound", http.StatusNotFound, func(c *client.Client) error {
			_, err := c.GetPolicy(context.Background(), id)
			return err
		}},
		{"CreatePolicy_BadRequest", http.StatusBadRequest, func(c *client.Client) error {
			_, err := c.CreatePolicy(context.Background(), client.PolicyRequest{})
			return err
		}},
		{"UpdatePolicy_NotFound", http.StatusNotFound, func(c *client.Client) error {
			_, err := c.UpdatePolicy(context.Background(), id, client.PolicyRequest{Name: "x"})
			return err
		}},
		{"DeletePolicy_NotFound", http.StatusNotFound, func(c *client.Client) error {
			return c.DeletePolicy(context.Background(), id)
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, tt.status, map[string]string{"error": "test error"})
			}))
			defer srv.Close()

			assertAPIError(t, tt.call(newTestClient(srv)), tt.status)
		})
	}
}

// --------------------------------------------------------------------------
// Shared helpers
// --------------------------------------------------------------------------

// checkAuth asserts the correct Authorization header is present.
func checkAuth(t *testing.T, r *http.Request) {
	t.Helper()
	auth := r.Header.Get("Authorization")
	expected := "Bearer " + testToken
	if auth != expected {
		t.Errorf("got Authorization %q, want %q", auth, expected)
	}
}

// assertAPIError asserts err is a *APIError with the given HTTP status code
// and returns it for further assertions on Body/Error().
func assertAPIError(t *testing.T, err error, wantStatus int) *client.APIError {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	var apiErr *client.APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("expected *client.APIError, got %T: %v", err, err)
	}
	if apiErr.Status != wantStatus {
		t.Errorf("got Status %d, want %d", apiErr.Status, wantStatus)
	}
	return apiErr
}
