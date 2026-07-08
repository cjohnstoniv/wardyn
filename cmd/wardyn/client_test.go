// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// These tests exercise the cmd/wardyn apiClient request-building against a
// hermetic httptest server. They assert the wire contract every CLI command
// depends on: HTTP method, URL path/query, the Authorization bearer header,
// the JSON request body, and how non-2xx responses surface as errors. The
// apiClient builds its own *http.Client internally, but an httptest.Server
// listens on real localhost so the default client reaches it without any
// injection — keeping the tests fully hermetic (no real network).

const testToken = "test-admin-token"

// capture is what each test server records about the single request it saw.
type capture struct {
	method string
	path   string
	rawURL string
	query  string
	auth   string
	ctype  string
	body   []byte
}

// newCapturingServer returns a test server that records the incoming request
// into *cap and replies with status/respBody. respBody may be nil for an empty
// body. The returned client is wired to the server's URL with testToken.
func newCapturingServer(t *testing.T, cap *capture, status int, respBody any) (*httptest.Server, *apiClient) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cap.method = r.Method
		cap.path = r.URL.Path
		cap.rawURL = r.URL.String()
		cap.query = r.URL.RawQuery
		cap.auth = r.Header.Get("Authorization")
		cap.ctype = r.Header.Get("Content-Type")
		cap.body, _ = readAll(r)
		if respBody != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_ = json.NewEncoder(w).Encode(respBody)
			return
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(srv.Close)
	return srv, &apiClient{baseURL: srv.URL, token: testToken}
}

// readAll drains a request body into bytes (kept in one spot).
func readAll(r *http.Request) ([]byte, error) {
	return io.ReadAll(r.Body)
}

// assertAuth checks the bearer header carried the configured token.
func assertAuth(t *testing.T, cap *capture) {
	t.Helper()
	want := "Bearer " + testToken
	if cap.auth != want {
		t.Errorf("Authorization = %q, want %q", cap.auth, want)
	}
}

// decodeBody unmarshals the captured JSON body into a generic map.
func decodeBody(t *testing.T, cap *capture) map[string]any {
	t.Helper()
	if len(cap.body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(cap.body, &m); err != nil {
		t.Fatalf("body is not JSON object: %v (raw %q)", err, cap.body)
	}
	return m
}

// --------------------------------------------------------------------------
// no-token (LOCAL HOST MODE): do() proceeds WITHOUT an Authorization header
// rather than erroring client-side, so the CLI works against a loopback wardynd
// in local mode. An auth-gated server returns a clear 401 instead.
// --------------------------------------------------------------------------

func TestClientDo_NoTokenSendsNoAuthHeader(t *testing.T) {
	var gotAuth string
	var sawReq bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawReq = true
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	c := &apiClient{baseURL: srv.URL} // no token
	if _, err := c.listRuns(context.Background()); err != nil {
		t.Fatalf("expected no-token request to proceed, got error: %v", err)
	}
	if !sawReq {
		t.Fatal("server saw no request; do() refused before sending")
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty (no token)", gotAuth)
	}
}

// --------------------------------------------------------------------------
// createRun: POST /api/v1/runs with the run body
// --------------------------------------------------------------------------

func TestClientCreateRun(t *testing.T) {
	id := uuid.New()
	want := types.AgentRun{
		ID:               id,
		Agent:            "claude-code",
		Repo:             "org/repo",
		Task:             "fix bug",
		ConfinementClass: types.CC2,
		State:            types.RunPending,
		SPIFFEID:         "spiffe://example/agent-run/x",
	}
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusCreated, want)

	got, err := c.createRun(context.Background(), createRunBody{
		Agent: "claude-code", Repo: "org/repo", Task: "fix bug",
		PolicyID: "pol-1", ConfinementClass: "CC2", Interactive: true,
	})
	if err != nil {
		t.Fatalf("createRun returned error: %v", err)
	}
	if cap.method != http.MethodPost {
		t.Errorf("method = %s, want POST", cap.method)
	}
	if cap.path != "/api/v1/runs" {
		t.Errorf("path = %s, want /api/v1/runs", cap.path)
	}
	if cap.ctype != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", cap.ctype)
	}
	assertAuth(t, &cap)
	body := decodeBody(t, &cap)
	if body["agent"] != "claude-code" || body["repo"] != "org/repo" || body["task"] != "fix bug" {
		t.Errorf("body agent/repo/task wrong: %v", body)
	}
	if body["policy_id"] != "pol-1" || body["confinement_class"] != "CC2" {
		t.Errorf("body policy/confinement wrong: %v", body)
	}
	if body["interactive"] != true {
		t.Errorf("body interactive = %v, want true", body["interactive"])
	}
	if got.ID != id {
		t.Errorf("decoded run ID = %v, want %v", got.ID, id)
	}
}

// createRunBody omits empty optional fields so a minimal run does not send
// policy_id / confinement_class / interactive on the wire.
func TestClientCreateRun_OmitsEmptyOptionals(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusCreated, types.AgentRun{ID: uuid.New()})

	if _, err := c.createRun(context.Background(), createRunBody{Agent: "a", Repo: "o/r"}); err != nil {
		t.Fatalf("createRun returned error: %v", err)
	}
	body := decodeBody(t, &cap)
	for _, k := range []string{"policy_id", "confinement_class", "interactive"} {
		if _, present := body[k]; present {
			t.Errorf("body should omit empty %q, got %v", k, body[k])
		}
	}
}

// --------------------------------------------------------------------------
// listRuns / killRun
// --------------------------------------------------------------------------

func TestClientListRuns(t *testing.T) {
	want := []types.AgentRun{{ID: uuid.New(), Agent: "a"}, {ID: uuid.New(), Agent: "b"}}
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusOK, want)

	got, err := c.listRuns(context.Background())
	if err != nil {
		t.Fatalf("listRuns returned error: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/api/v1/runs" {
		t.Errorf("got %s %s, want GET /api/v1/runs", cap.method, cap.path)
	}
	if len(cap.body) != 0 {
		t.Errorf("GET should send no body, got %q", cap.body)
	}
	if len(got) != 2 {
		t.Errorf("decoded %d runs, want 2", len(got))
	}
}

func TestClientKillRun(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusAccepted, nil)

	if err := c.killRun(context.Background(), "run-123"); err != nil {
		t.Fatalf("killRun returned error: %v", err)
	}
	if cap.method != http.MethodPost || cap.path != "/api/v1/runs/run-123/kill" {
		t.Errorf("got %s %s, want POST /api/v1/runs/run-123/kill", cap.method, cap.path)
	}
}

// --------------------------------------------------------------------------
// approvals: list (state query) + approve/deny verb + reason body
// --------------------------------------------------------------------------

func TestClientListApprovals_WithState(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusOK, []types.ApprovalRequest{})

	if _, err := c.listApprovals(context.Background(), "PENDING"); err != nil {
		t.Fatalf("listApprovals returned error: %v", err)
	}
	if cap.path != "/api/v1/approvals" {
		t.Errorf("path = %s, want /api/v1/approvals", cap.path)
	}
	if cap.query != "state=PENDING" {
		t.Errorf("query = %q, want state=PENDING", cap.query)
	}
}

func TestClientListApprovals_NoState(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusOK, []types.ApprovalRequest{})

	if _, err := c.listApprovals(context.Background(), ""); err != nil {
		t.Fatalf("listApprovals returned error: %v", err)
	}
	if cap.query != "" {
		t.Errorf("query = %q, want empty when state unset", cap.query)
	}
}

func TestClientDecideApproval_Verb(t *testing.T) {
	tests := []struct {
		name     string
		approve  bool
		wantVerb string
	}{
		{name: "approve hits the approve verb", approve: true, wantVerb: "approve"},
		{name: "deny hits the deny verb", approve: false, wantVerb: "deny"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cap capture
			_, c := newCapturingServer(t, &cap, http.StatusOK, types.ApprovalRequest{State: types.ApprovalApproved})

			if _, err := c.decideApproval(context.Background(), "ap-7", tc.approve, "looks fine"); err != nil {
				t.Fatalf("decideApproval returned error: %v", err)
			}
			wantPath := "/api/v1/approvals/ap-7/" + tc.wantVerb
			if cap.method != http.MethodPost || cap.path != wantPath {
				t.Errorf("got %s %s, want POST %s", cap.method, cap.path, wantPath)
			}
			body := decodeBody(t, &cap)
			if body["reason"] != "looks fine" {
				t.Errorf("reason body = %v, want %q", body["reason"], "looks fine")
			}
		})
	}
}

// --------------------------------------------------------------------------
// audit: GET /api/v1/audit?run_id=...
// --------------------------------------------------------------------------

func TestClientAudit(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusOK, []types.AuditEvent{{Action: "run.create"}})

	if _, err := c.audit(context.Background(), "run-9"); err != nil {
		t.Fatalf("audit returned error: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/api/v1/audit" {
		t.Errorf("got %s %s, want GET /api/v1/audit", cap.method, cap.path)
	}
	if cap.query != "run_id=run-9" {
		t.Errorf("query = %q, want run_id=run-9", cap.query)
	}
}

// --------------------------------------------------------------------------
// policy CRUD: list/get/create/update/delete method+path+body
// --------------------------------------------------------------------------

func TestClientPolicyCRUD(t *testing.T) {
	tests := []struct {
		name       string
		call       func(c *apiClient) error
		wantMethod string
		wantPath   string
		wantBody   bool
	}{
		{
			name:       "list",
			call:       func(c *apiClient) error { _, err := c.listPolicies(context.Background()); return err },
			wantMethod: http.MethodGet, wantPath: "/api/v1/policies",
		},
		{
			name:       "get",
			call:       func(c *apiClient) error { _, err := c.getPolicy(context.Background(), "p-1"); return err },
			wantMethod: http.MethodGet, wantPath: "/api/v1/policies/p-1",
		},
		{
			name: "create",
			call: func(c *apiClient) error {
				_, err := c.createPolicy(context.Background(), policyBody{Name: "default"})
				return err
			},
			wantMethod: http.MethodPost, wantPath: "/api/v1/policies", wantBody: true,
		},
		{
			name: "update",
			call: func(c *apiClient) error {
				_, err := c.updatePolicy(context.Background(), "p-2", policyBody{Name: "renamed"})
				return err
			},
			wantMethod: http.MethodPut, wantPath: "/api/v1/policies/p-2", wantBody: true,
		},
		{
			name:       "delete",
			call:       func(c *apiClient) error { return c.deletePolicy(context.Background(), "p-3") },
			wantMethod: http.MethodDelete, wantPath: "/api/v1/policies/p-3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var cap capture
			// We only assert the outbound request here (method/path/body), so the
			// server returns an empty 200 with no body — that decodes cleanly into
			// any of the slice/object return types these calls expect.
			_, c := newCapturingServer(t, &cap, http.StatusOK, nil)

			if err := tc.call(c); err != nil {
				t.Fatalf("%s call returned error: %v", tc.name, err)
			}
			if cap.method != tc.wantMethod || cap.path != tc.wantPath {
				t.Errorf("got %s %s, want %s %s", cap.method, cap.path, tc.wantMethod, tc.wantPath)
			}
			assertAuth(t, &cap)
			if tc.wantBody && len(cap.body) == 0 {
				t.Errorf("%s should send a JSON body, got none", tc.name)
			}
			if !tc.wantBody && len(cap.body) != 0 {
				t.Errorf("%s should send no body, got %q", tc.name, cap.body)
			}
		})
	}
}

func TestClientCreatePolicy_BodyShape(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusOK, types.RunPolicy{})

	spec := types.RunPolicySpec{
		AllowedDomains:      []string{"github.com"},
		MinConfinementClass: types.CC2,
		FirstUseApproval:    types.FirstUseDenyWithReview,
	}
	if _, err := c.createPolicy(context.Background(), policyBody{Name: "p", Spec: spec}); err != nil {
		t.Fatalf("createPolicy returned error: %v", err)
	}
	body := decodeBody(t, &cap)
	if body["name"] != "p" {
		t.Errorf("body name = %v, want p", body["name"])
	}
	rawSpec, ok := body["spec"].(map[string]any)
	if !ok {
		t.Fatalf("body spec missing/not an object: %v", body["spec"])
	}
	if rawSpec["min_confinement_class"] != "CC2" {
		t.Errorf("spec min_confinement_class = %v, want CC2", rawSpec["min_confinement_class"])
	}
	if rawSpec["first_use_approval"] != "deny_with_review" {
		t.Errorf("spec first_use_approval = %v, want deny_with_review", rawSpec["first_use_approval"])
	}
}

// --------------------------------------------------------------------------
// secrets: put (value body), delete, list (names extraction)
// --------------------------------------------------------------------------

func TestClientPutSecret(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusNoContent, nil)

	// A multi-line value must round-trip verbatim in the JSON body — this is
	// the same multi-line-secret concern the readSecretValue fix addresses,
	// asserted here at the wire layer.
	val := "-----BEGIN KEY-----\nabc\ndef\n-----END KEY-----"
	if err := c.putSecret(context.Background(), "gh-token", val); err != nil {
		t.Fatalf("putSecret returned error: %v", err)
	}
	if cap.method != http.MethodPut || cap.path != "/api/v1/secrets/gh-token" {
		t.Errorf("got %s %s, want PUT /api/v1/secrets/gh-token", cap.method, cap.path)
	}
	body := decodeBody(t, &cap)
	if body["value"] != val {
		t.Errorf("body value = %q, want %q (multi-line must round-trip)", body["value"], val)
	}
}

func TestClientDeleteSecret(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusNoContent, nil)

	if err := c.deleteSecret(context.Background(), "gh-token"); err != nil {
		t.Fatalf("deleteSecret returned error: %v", err)
	}
	if cap.method != http.MethodDelete || cap.path != "/api/v1/secrets/gh-token" {
		t.Errorf("got %s %s, want DELETE /api/v1/secrets/gh-token", cap.method, cap.path)
	}
}

func TestClientListSecrets(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusOK, map[string][]string{"names": {"a", "b", "c"}})

	got, err := c.listSecrets(context.Background())
	if err != nil {
		t.Fatalf("listSecrets returned error: %v", err)
	}
	if cap.method != http.MethodGet || cap.path != "/api/v1/secrets" {
		t.Errorf("got %s %s, want GET /api/v1/secrets", cap.method, cap.path)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("names = %v, want [a b c]", got)
	}
}

// --------------------------------------------------------------------------
// error surfacing: a >=400 response becomes an error carrying method/path/status
// --------------------------------------------------------------------------

func TestClientDo_APIErrorSurfaced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"agent and repo are required"}`))
	}))
	t.Cleanup(srv.Close)
	c := &apiClient{baseURL: srv.URL, token: testToken}

	_, err := c.createRun(context.Background(), createRunBody{})
	if err == nil {
		t.Fatal("expected error on 400 response, got nil")
	}
	msg := err.Error()
	for _, want := range []string{"POST", "/api/v1/runs", "400", "agent and repo are required"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q missing %q", msg, want)
		}
	}
}

func TestClientDo_BaseURLTrailingSlashTrimmed(t *testing.T) {
	var cap capture
	srv, _ := newCapturingServer(t, &cap, http.StatusOK, []types.AgentRun{})
	// Construct a client whose baseURL has a trailing slash; the path must not
	// get a doubled slash.
	c := &apiClient{baseURL: srv.URL + "/", token: testToken}

	if _, err := c.listRuns(context.Background()); err != nil {
		t.Fatalf("listRuns returned error: %v", err)
	}
	if cap.path != "/api/v1/runs" {
		t.Errorf("path = %s, want /api/v1/runs (no doubled slash)", cap.path)
	}
}
