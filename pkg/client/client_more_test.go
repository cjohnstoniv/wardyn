// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client_test

// client_more_test.go fills the gaps left by client_test.go: the ERROR half of
// the 2 KiB body-limit regression, context-cancellation propagation, typed-error
// decode (well-formed and malformed bodies), and the policy/grant methods that
// the original suite left uncovered. It reuses the shared helpers
// (newTestClient, writeJSON, checkAuth, assertAPIError, testToken) defined in
// client_test.go — same external test package.

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

// --------------------------------------------------------------------------
// Body-limit regression — ERROR side (HIGH)
// --------------------------------------------------------------------------

// TestErrorBody_CappedAt2KiB is the companion to
// TestSuccessBody_LargerThan2KiB_DecodesFully: the 2 KiB LimitReader must STILL
// apply to error (non-2xx) bodies so a hostile or runaway server cannot exhaust
// client memory through an oversized error response. A >2 KiB error body must be
// truncated to exactly 2048 bytes in APIError.Body, while the status is
// preserved verbatim.
func TestErrorBody_CappedAt2KiB(t *testing.T) {
	// A 5000-byte body comfortably exceeds the 2048 cap; using a single repeated
	// rune makes the truncation length trivial to assert.
	hugeBody := strings.Repeat("E", 5000)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checkAuth(t, r)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(hugeBody))
	}))
	defer srv.Close()

	_, err := newTestClient(srv).ListRuns(context.Background())
	var apiErr *client.APIError
	assertAsAPIError(t, err, &apiErr)
	if apiErr.Status != http.StatusInternalServerError {
		t.Errorf("got Status %d, want %d", apiErr.Status, http.StatusInternalServerError)
	}
	if len(apiErr.Body) != 2048 {
		t.Errorf("error body not capped: got %d bytes, want exactly 2048", len(apiErr.Body))
	}
	if apiErr.Body != strings.Repeat("E", 2048) {
		t.Errorf("error body content is not the first 2048 bytes of the response")
	}
}

// TestErrorBody_SmallerThan2KiB_NotTruncated confirms the cap is a CEILING, not
// a fixed pad: an error body under 2 KiB is returned in full, untouched.
func TestErrorBody_SmallerThan2KiB_NotTruncated(t *testing.T) {
	const small = `{"error":"agent and repo are required"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "agent and repo are required"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).CreateRun(context.Background(), client.CreateRunRequest{})
	var apiErr *client.APIError
	assertAsAPIError(t, err, &apiErr)
	// writeJSON appends a trailing newline via json.Encoder; compare on the
	// JSON-meaningful content rather than exact bytes.
	if !strings.Contains(apiErr.Body, small) {
		t.Errorf("got body %q, want it to contain %q", apiErr.Body, small)
	}
	if len(apiErr.Body) >= 2048 {
		t.Errorf("small error body should be well under the cap, got %d bytes", len(apiErr.Body))
	}
}

// --------------------------------------------------------------------------
// Typed-error decode (well-formed + malformed)
// --------------------------------------------------------------------------

// TestTypedError_JSONBodyPreserved asserts a non-2xx response with a JSON error
// body yields a *APIError whose Status matches and whose Body carries the raw
// JSON verbatim (the SDK does not parse the error envelope — it preserves it for
// the caller / diagnostic display).
func TestTypedError_JSONBodyPreserved(t *testing.T) {
	const errJSON = `{"error":"policy spec invalid","detail":"min_confinement_class unknown"}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = w.Write([]byte(errJSON))
	}))
	defer srv.Close()

	_, err := newTestClient(srv).ListPolicies(context.Background())
	var apiErr *client.APIError
	assertAsAPIError(t, err, &apiErr)
	if apiErr.Status != http.StatusUnprocessableEntity {
		t.Errorf("got Status %d, want 422", apiErr.Status)
	}
	if apiErr.Body != errJSON {
		t.Errorf("got Body %q, want %q", apiErr.Body, errJSON)
	}
	// The Error() string must surface both the status and the body for ops.
	if !strings.Contains(apiErr.Error(), "422") || !strings.Contains(apiErr.Error(), "policy spec invalid") {
		t.Errorf("Error() string missing status or body: %q", apiErr.Error())
	}
}

// TestTypedError_MalformedBodyDegradesGracefully asserts a non-2xx response
// whose body is NOT valid JSON still produces a usable *APIError (status +
// raw body) rather than a decode failure. The error path never JSON-decodes the
// body, so garbage in -> APIError with garbage Body out, never a panic or a
// confusing "decode response" error.
func TestTypedError_MalformedBodyDegradesGracefully(t *testing.T) {
	const garbage = "<html><body>502 Bad Gateway</body></html>"

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(garbage))
	}))
	defer srv.Close()

	_, err := newTestClient(srv).ListRuns(context.Background())
	var apiErr *client.APIError
	assertAsAPIError(t, err, &apiErr)
	if apiErr.Status != http.StatusBadGateway {
		t.Errorf("got Status %d, want 502", apiErr.Status)
	}
	if apiErr.Body != garbage {
		t.Errorf("got Body %q, want the raw non-JSON body %q", apiErr.Body, garbage)
	}
	// Crucially, the error is *APIError, NOT a "wardyn: decode response" error.
	if strings.Contains(err.Error(), "decode response") {
		t.Errorf("malformed error body must not surface as a decode error: %v", err)
	}
}

// TestTypedError_EmptyBody asserts a non-2xx with NO body still yields a
// *APIError carrying the status and an empty Body (no decode attempted).
func TestTypedError_EmptyBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	_, err := newTestClient(srv).ListRuns(context.Background())
	var apiErr *client.APIError
	assertAsAPIError(t, err, &apiErr)
	if apiErr.Status != http.StatusForbidden {
		t.Errorf("got Status %d, want 403", apiErr.Status)
	}
	if apiErr.Body != "" {
		t.Errorf("got Body %q, want empty", apiErr.Body)
	}
}

// --------------------------------------------------------------------------
// Context cancellation propagation
// --------------------------------------------------------------------------

// TestContextCanceled_ReturnsCtxError asserts that an already-canceled context
// makes the request fail with the context's error (context.Canceled), surfaced
// wrapped in the SDK's "wardyn: http:" error — NOT a *APIError. A canceled call
// must never look like a server response.
func TestContextCanceled_ReturnsCtxError(t *testing.T) {
	// The server should never actually serve this request; if cancellation
	// works, hc.Do returns before the handler runs.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler should not be reached for a pre-canceled context")
		writeJSON(w, http.StatusOK, []types.AgentRun{})
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel BEFORE the call so hc.Do fails immediately.

	_, err := newTestClient(srv).ListRuns(ctx)
	if err == nil {
		t.Fatal("expected error from canceled context, got nil")
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("canceled context must not yield *APIError, got %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error does not wrap context.Canceled: %v", err)
	}
}

// TestContextDeadlineExceeded_Propagates asserts a deadline that elapses while
// the server is hanging surfaces context.DeadlineExceeded (again wrapped, not an
// *APIError). This exercises the in-flight cancellation path rather than the
// pre-canceled fast path.
func TestContextDeadlineExceeded_Propagates(t *testing.T) {
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block // hang until the test releases us, well past the deadline.
		writeJSON(w, http.StatusOK, []types.AgentRun{})
	}))
	defer srv.Close()
	defer close(block) // let the blocked handler return at test teardown.

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := newTestClient(srv).ListRuns(ctx)
	if err == nil {
		t.Fatal("expected deadline error, got nil")
	}
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		t.Fatalf("deadline must not yield *APIError, got %v", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("error does not wrap context.DeadlineExceeded: %v", err)
	}
}

// --------------------------------------------------------------------------
// ListGrants (previously uncovered)
// --------------------------------------------------------------------------

func TestListGrants_Success(t *testing.T) {
	runID := uuid.New()
	grants := []types.CredentialGrant{
		{ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{Kind: types.GrantGitHubToken, TTLSeconds: 3600}},
		{ID: uuid.New(), RunID: runID, Spec: types.GrantSpec{Kind: types.GrantAPIKey}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runs/"+runID.String()+"/grants" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, grants)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListGrants(context.Background(), runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d grants, want 2", len(got))
	}
	if got[0].Spec.Kind != types.GrantGitHubToken {
		t.Errorf("got grant kind %q, want github_token", got[0].Spec.Kind)
	}
	if got[0].Spec.TTLSeconds != 3600 {
		t.Errorf("got TTL %d, want 3600", got[0].Spec.TTLSeconds)
	}
}

func TestListGrants_NotFound(t *testing.T) {
	runID := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "run not found"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).ListGrants(context.Background(), runID)
	assertAPIError(t, err, http.StatusNotFound)
}

// --------------------------------------------------------------------------
// ListPolicies / GetPolicy (previously uncovered)
// --------------------------------------------------------------------------

func TestListPolicies_Success(t *testing.T) {
	policies := []types.RunPolicy{
		{ID: uuid.New(), Name: "default", Spec: types.RunPolicySpec{MinConfinementClass: types.CC1}},
		{ID: uuid.New(), Name: "strict", Spec: types.RunPolicySpec{MinConfinementClass: types.CC3}},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/policies" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, policies)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListPolicies(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d policies, want 2", len(got))
	}
	if got[1].Name != "strict" || got[1].Spec.MinConfinementClass != types.CC3 {
		t.Errorf("got policy[1] %+v, want name=strict class=CC3", got[1])
	}
}

func TestGetPolicy_Success(t *testing.T) {
	id := uuid.New()
	want := types.RunPolicy{ID: id, Name: "default", Spec: types.RunPolicySpec{
		AllowedDomains:      []string{"github.com", "*.githubusercontent.com"},
		MinConfinementClass: types.CC2,
	}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/policies/"+id.String() {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).GetPolicy(context.Background(), id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != id || got.Name != "default" {
		t.Errorf("got %+v, want id=%v name=default", got, id)
	}
	if len(got.Spec.AllowedDomains) != 2 || got.Spec.AllowedDomains[0] != "github.com" {
		t.Errorf("got allowed_domains %v, want [github.com *.githubusercontent.com]", got.Spec.AllowedDomains)
	}
}

func TestGetPolicy_NotFound(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "policy not found"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).GetPolicy(context.Background(), id)
	assertAPIError(t, err, http.StatusNotFound)
}

// --------------------------------------------------------------------------
// CreatePolicy (previously uncovered)
// --------------------------------------------------------------------------

// TestCreatePolicy_Success asserts CreatePolicy POSTs to /api/v1/policies with
// the auth header, sends {name, spec} on the wire (spec nouns intact), and
// decodes the 201 RunPolicy response.
func TestCreatePolicy_Success(t *testing.T) {
	id := uuid.New()
	want := types.RunPolicy{ID: id, Name: "ci-policy", Spec: types.RunPolicySpec{MinConfinementClass: types.CC2}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/policies" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		if ct := r.Header.Get("Content-Type"); ct != "application/json" {
			t.Errorf("got Content-Type %q, want application/json", ct)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["name"] != "ci-policy" {
			t.Errorf("got name %v, want ci-policy", body["name"])
		}
		spec, ok := body["spec"].(map[string]any)
		if !ok {
			t.Fatalf("spec missing or wrong type: %v", body["spec"])
		}
		if spec["min_confinement_class"] != "CC2" {
			t.Errorf("got spec.min_confinement_class %v, want CC2", spec["min_confinement_class"])
		}
		writeJSON(w, http.StatusCreated, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).CreatePolicy(context.Background(), client.PolicyRequest{
		Name: "ci-policy",
		Spec: client.RunPolicySpec{MinConfinementClass: client.CC2},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.ID != id || got.Name != "ci-policy" {
		t.Errorf("got %+v, want id=%v name=ci-policy", got, id)
	}
}

func TestCreatePolicy_BadRequest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "name is required"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).CreatePolicy(context.Background(), client.PolicyRequest{})
	assertAPIError(t, err, http.StatusBadRequest)
}

// --------------------------------------------------------------------------
// UpdatePolicy (previously uncovered)
// --------------------------------------------------------------------------

// TestUpdatePolicy_Success asserts UpdatePolicy PUTs to
// /api/v1/policies/{id} with the body and decodes the updated RunPolicy.
func TestUpdatePolicy_Success(t *testing.T) {
	id := uuid.New()
	want := types.RunPolicy{ID: id, Name: "renamed", Spec: types.RunPolicySpec{MinConfinementClass: types.CC3}}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/policies/"+id.String() {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode body: %v", err)
		}
		if body["name"] != "renamed" {
			t.Errorf("got name %v, want renamed", body["name"])
		}
		writeJSON(w, http.StatusOK, want)
	}))
	defer srv.Close()

	got, err := newTestClient(srv).UpdatePolicy(context.Background(), id, client.PolicyRequest{
		Name: "renamed",
		Spec: client.RunPolicySpec{MinConfinementClass: client.CC3},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "renamed" || got.Spec.MinConfinementClass != types.CC3 {
		t.Errorf("got %+v, want name=renamed class=CC3", got)
	}
}

func TestUpdatePolicy_NotFound(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "policy not found"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).UpdatePolicy(context.Background(), id, client.PolicyRequest{Name: "x"})
	assertAPIError(t, err, http.StatusNotFound)
}

// --------------------------------------------------------------------------
// DeletePolicy (previously uncovered)
// --------------------------------------------------------------------------

// TestDeletePolicy_Success asserts DeletePolicy issues a DELETE to
// /api/v1/policies/{id} and returns nil on a 204 (empty body, no decode).
func TestDeletePolicy_Success(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/policies/"+id.String() {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	if err := newTestClient(srv).DeletePolicy(context.Background(), id); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeletePolicy_NotFound(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "policy not found"})
	}))
	defer srv.Close()

	err := newTestClient(srv).DeletePolicy(context.Background(), id)
	assertAPIError(t, err, http.StatusNotFound)
}

// --------------------------------------------------------------------------
// Local helper: assertAsAPIError centralizes the errors.As + fatal dance and
// binds the matched *APIError into the caller's variable for further assertions.
// --------------------------------------------------------------------------

// assertAsAPIError fails the test unless err is a *client.APIError, binding it
// into target on success.
func assertAsAPIError(t *testing.T, err error, target **client.APIError) {
	t.Helper()
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !errors.As(err, target) {
		t.Fatalf("expected *client.APIError, got %T: %v", err, err)
	}
}
