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
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
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
	apiErr := assertAPIError(t, err, http.StatusInternalServerError)
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
	apiErr := assertAPIError(t, err, http.StatusBadRequest)
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
	apiErr := assertAPIError(t, err, http.StatusUnprocessableEntity)
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
	apiErr := assertAPIError(t, err, http.StatusBadGateway)
	if apiErr.Body != garbage {
		t.Errorf("got Body %q, want the raw non-JSON body %q", apiErr.Body, garbage)
	}
	// Crucially, the error is *APIError, NOT a "decode response" error.
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
	apiErr := assertAPIError(t, err, http.StatusForbidden)
	if apiErr.Body != "" {
		t.Errorf("got Body %q, want empty", apiErr.Body)
	}
}

// --------------------------------------------------------------------------
// Context cancellation propagation
// --------------------------------------------------------------------------

// TestContextCanceled_ReturnsCtxError asserts that an already-canceled context
// makes the request fail with the context's error (context.Canceled), surfaced
// wrapped in the SDK's "http:" error — NOT a *APIError. A canceled call
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

// TestDeleteSource_ReturnsDetachedFrom is the W6-S1-2 regression: a forced
// delete's response used to be discarded entirely (out=nil), so the CLI had
// no way to tell the operator which workspaces it just detached from — the
// only signal available, since nothing 422s downstream at run time. The
// server answers 200 with {"detached_from": [...]}; the SDK must surface it.
func TestDeleteSource_ReturnsDetachedFrom(t *testing.T) {
	id := uuid.New()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodDelete || r.URL.Path != "/api/v1/sources/"+id.String() {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		if r.URL.Query().Get("force") != "1" {
			t.Errorf("query = %q, want force=1", r.URL.RawQuery)
		}
		checkAuth(t, r)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"detached_from":["payments-ws","review-ws"]}`))
	}))
	defer srv.Close()

	detachedFrom, err := newTestClient(srv).DeleteSource(context.Background(), id, true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if want := []string{"payments-ws", "review-ws"}; !slices.Equal(detachedFrom, want) {
		t.Errorf("detachedFrom = %v, want %v", detachedFrom, want)
	}
}

// GetRecording streams raw asciicast bytes (not JSON) and must still carry the
// bearer, since do()'s JSON path is bypassed.
func TestGetRecording_StreamsCastWithAuth(t *testing.T) {
	id := uuid.New()
	want := "{\"version\":2}\n[0.1,\"o\",\"hi\"]\n"
	var gotPath, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotAuth = r.URL.Path, r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/x-asciicast")
		_, _ = io.WriteString(w, want)
	}))
	defer srv.Close()

	rc, err := newTestClient(srv).GetRecording(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRecording: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if string(got) != want {
		t.Errorf("cast = %q, want %q", got, want)
	}
	// The doubled id is the real route shape (mounted per-run, handler takes the
	// recording id) — the SDK hides it, so pin it.
	if wantPath := "/api/v1/runs/" + id.String() + "/recording/" + id.String(); gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotAuth == "" {
		t.Error("Authorization header missing on the raw-stream path")
	}
}

// W21-S1-6: an interactive run can carry MULTIPLE recordings, one per attach
// session, each stored under the composite key "<run-id>~<session>"
// (internal/recording.CastKey) — the server has always served that shape, but
// GetRecording hardcoded the cast key to the bare run id, so nothing on the
// CLI/SDK side could ever reach any recording but the run's own. The optional
// session argument composes the SAME key the server expects.
func TestGetRecording_SessionArgUsesCompositeKey(t *testing.T) {
	id := uuid.New()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-asciicast")
		_, _ = io.WriteString(w, "{\"version\":2}\n")
	}))
	defer srv.Close()

	rc, err := newTestClient(srv).GetRecording(context.Background(), id, "attach-1")
	if err != nil {
		t.Fatalf("GetRecording: %v", err)
	}
	defer rc.Close()
	_, _ = io.ReadAll(rc)

	if want := "/api/v1/runs/" + id.String() + "/recording/" + id.String() + "~attach-1"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// A zero-arg call (every existing caller) must keep composing the bare-id
// key, byte-identical to before session was added — TestGetRecording_
// StreamsCastWithAuth above already pins this, but that test predates session
// existing at all; pin it explicitly here too so a regression in the
// variadic's zero-length branch specifically is caught by name.
func TestGetRecording_NoSessionArgDefaultsToBareRunID(t *testing.T) {
	id := uuid.New()
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/x-asciicast")
		_, _ = io.WriteString(w, "{\"version\":2}\n")
	}))
	defer srv.Close()

	rc, err := newTestClient(srv).GetRecording(context.Background(), id)
	if err != nil {
		t.Fatalf("GetRecording: %v", err)
	}
	defer rc.Close()
	_, _ = io.ReadAll(rc)

	if want := "/api/v1/runs/" + id.String() + "/recording/" + id.String(); gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
}

// TestPutSiteConfig_StripsIntegrations pins PLATFORM-API-5: the server
// hard-rejects a PUT /site-config body carrying a non-empty integrations (they
// are managed through their own endpoints), so the documented disaster-recovery
// round-trip (`wardyn site-config get > f` before a reset, `wardyn site-config
// apply f` after) must not resend whatever GetSiteConfig returned verbatim —
// PutSiteConfig strips it, once, so no caller has to remember to.
func TestPutSiteConfig_StripsIntegrations(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v1/site-config" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		writeJSON(w, http.StatusOK, types.SiteConfig{})
	}))
	defer srv.Close()

	captured := types.SiteConfig{
		ScmHosts: []string{"github.example.com"},
		Integrations: []types.Integration{
			{ID: "acme-anthropic", Kind: types.IntegrationKindAnthropicAPIKey},
		},
	}
	if _, _, err := newTestClient(srv).PutSiteConfig(context.Background(), captured); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, present := gotBody["integrations"]; present {
		t.Errorf("integrations must be stripped from the request body, got: %v", gotBody["integrations"])
	}
	if gotBody["scm_hosts"] == nil {
		t.Error("stripping integrations must not touch the rest of the document")
	}
	// The caller's own copy must be untouched (cfg is passed by value, but
	// prove it): a caller reusing `captured` afterward (e.g. to print what it
	// meant to send) must not find it silently emptied.
	if len(captured.Integrations) != 1 {
		t.Errorf("PutSiteConfig must not mutate the caller's SiteConfig, got %d integrations", len(captured.Integrations))
	}
}

// --------------------------------------------------------------------------
// SSH keys: ListSSHKeys / AddSSHKey
// --------------------------------------------------------------------------

func TestListSSHKeys_Decodes(t *testing.T) {
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/me/ssh-keys" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, []client.SSHPublicKey{
			{Fingerprint: "SHA256:abc", Principal: "alice@example.com", Name: "laptop", PublicKey: "ssh-ed25519 AAAA... laptop", Role: "member", CreatedAt: created},
		})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).ListSSHKeys(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Fingerprint != "SHA256:abc" || got[0].Name != "laptop" || got[0].Role != "member" {
		t.Errorf("got %+v, want one decoded key", got)
	}
	if !got[0].CreatedAt.Equal(created) {
		t.Errorf("created_at = %v, want %v", got[0].CreatedAt, created)
	}
}

// TestAddSSHKey_RequestBodyExactShapeAndDecodes pins the wire contract: the
// request carries EXACTLY {name, public_key} (no fingerprint, no principal —
// both are server-computed, never client-supplied), and a 201 decodes into
// the created record.
func TestAddSSHKey_RequestBodyExactShapeAndDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/me/ssh-keys" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("body not JSON: %v", err)
		}
		if len(body) != 2 || body["name"] != "laptop" || body["public_key"] != "ssh-ed25519 AAAA... laptop" {
			t.Errorf("body = %v, want exactly {name, public_key}", body)
		}
		writeJSON(w, http.StatusCreated, client.SSHPublicKey{
			Fingerprint: "SHA256:abc", Principal: "alice@example.com", Name: "laptop",
			PublicKey: "ssh-ed25519 AAAA... laptop", Role: "member",
		})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).AddSSHKey(context.Background(), "laptop", "ssh-ed25519 AAAA... laptop")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Fingerprint != "SHA256:abc" || got.Name != "laptop" {
		t.Errorf("got %+v, want the decoded created key", got)
	}
}

// TestAddSSHKey_422BecomesTypedErrorWithServerMessage: the handler's specific,
// actionable refusal text (e.g. "this looks like a PRIVATE key...") must
// survive as a typed *client.APIError a caller can act on, not collapse into
// an opaque status code.
func TestAddSSHKey_422BecomesTypedErrorWithServerMessage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{
			"error": "not a valid SSH public key: ssh: no key found",
		})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).AddSSHKey(context.Background(), "laptop", "not-a-key")
	apiErr := assertAPIError(t, err, http.StatusUnprocessableEntity)
	if !strings.Contains(apiErr.Error(), "not a valid SSH public key") {
		t.Errorf("Error() = %q, want it to carry the server's message", apiErr.Error())
	}
}

// --------------------------------------------------------------------------
// RunFiles
// --------------------------------------------------------------------------

func TestRunFiles_Decodes(t *testing.T) {
	runID := uuid.New()
	added, deleted := 12, 3
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/v1/runs/"+runID.String()+"/files" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
		}
		checkAuth(t, r)
		writeJSON(w, http.StatusOK, client.RunFiles{
			VCS:  "git",
			Path: "/home/agent/work",
			Files: []client.RunFileStat{
				{Path: "main.go", Status: "M", Added: &added, Deleted: &deleted},
				{Path: "logo.png", Status: "A", Binary: true},
			},
			Truncated: true,
		})
	}))
	defer srv.Close()

	got, err := newTestClient(srv).RunFiles(context.Background(), runID)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.VCS != "git" || got.Path != "/home/agent/work" || !got.Truncated {
		t.Errorf("got %+v, want vcs=git path=/home/agent/work truncated=true", got)
	}
	if len(got.Files) != 2 || got.Files[0].Path != "main.go" || *got.Files[0].Added != 12 || *got.Files[0].Deleted != 3 {
		t.Errorf("files[0] = %+v, want main.go +12/-3", got.Files[0])
	}
	if !got.Files[1].Binary || got.Files[1].Added != nil {
		t.Errorf("files[1] = %+v, want a binary file with no counts (never a confident +0/-0)", got.Files[1])
	}
}

// TestRunFiles_409Surfaces: a run with no sandbox yet (or a finished one) 409s
// rather than inventing an empty file list.
func TestRunFiles_409Surfaces(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "run has no sandbox to read (state=PENDING)"})
	}))
	defer srv.Close()

	_, err := newTestClient(srv).RunFiles(context.Background(), uuid.New())
	apiErr := assertAPIError(t, err, http.StatusConflict)
	if !strings.Contains(apiErr.Error(), "run has no sandbox to read") {
		t.Errorf("Error() = %q, want the server's message", apiErr.Error())
	}
}
