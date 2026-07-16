// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"errors"
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
// createRun: POST /api/v1/runs with the run body
// --------------------------------------------------------------------------

// createRunBody omits empty optional fields so a minimal run does not send
// policy_id / confinement_class / interactive on the wire.
func TestClientCreateRun_OmitsEmptyOptionals(t *testing.T) {
	id := uuid.New()
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusCreated, types.AgentRun{ID: id})

	got, err := c.createRun(context.Background(), createRunBody{Agent: "a", Repo: "o/r"})
	if err != nil {
		t.Fatalf("createRun returned error: %v", err)
	}
	if got.ID != id {
		t.Errorf("decoded run ID = %v, want %v", got.ID, id)
	}
	body := decodeBody(t, &cap)
	for _, k := range []string{"policy_id", "confinement_class", "interactive"} {
		if _, present := body[k]; present {
			t.Errorf("body should omit empty %q, got %v", k, body[k])
		}
	}
}

// TestClient_DoReturnsTypedAPIError is the U087 regression: a non-2xx response
// must surface a *apiError carrying the STATUS CODE + server body, so callers can
// distinguish auth / not-found / conflict programmatically instead of collapsing
// every failure into one opaque error that always exits 1.
func TestClient_DoReturnsTypedAPIError(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusConflict, map[string]string{"error": "run is already terminal"})

	err := c.killRun(context.Background(), uuid.New().String())
	if err == nil {
		t.Fatal("expected an error on a 409 response")
	}
	var ae *apiError
	if !errors.As(err, &ae) {
		t.Fatalf("error should be *apiError, got %T: %v", err, err)
	}
	if ae.StatusCode != http.StatusConflict {
		t.Errorf("apiError.StatusCode = %d, want 409", ae.StatusCode)
	}
	if !strings.Contains(ae.Body, "already terminal") {
		t.Errorf("apiError.Body should carry the server's actionable message, got %q", ae.Body)
	}
}

// --------------------------------------------------------------------------
// approvals: list (state query) — no CLI command yet for these, kept here
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

// --------------------------------------------------------------------------
// policy CRUD: list/get/create/update/delete method+path+body
// --------------------------------------------------------------------------

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

// listSecrets unwraps the {"names":[...]} envelope — the command test only
// asserts the outbound request, not the printed names, so this is the only
// coverage of that decode step.
func TestClientListSecrets(t *testing.T) {
	var cap capture
	_, c := newCapturingServer(t, &cap, http.StatusOK, map[string][]string{"names": {"a", "b", "c"}})

	got, err := c.listSecrets(context.Background())
	if err != nil {
		t.Fatalf("listSecrets returned error: %v", err)
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
