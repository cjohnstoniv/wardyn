// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package client is the public Go SDK for the Wardyn control plane.
//
// It mirrors the REST API surface of wardynd exactly — same paths, same
// status codes, same JSON vocabulary (internal/types is the shared source of
// truth). The SDK adds zero non-stdlib dependencies so it can be embedded in
// external tooling without dependency friction. (The in-repo `wardyn` CLI keeps
// its own minimal TRANSPORT in cmd/wardyn/client.go — for CLI-specific error and
// exit-code mapping — but posts THIS package's CreateRunRequest rather than
// redeclaring the body, so the two can never drift.)
//
// Usage:
//
//	c := client.New("https://wardyn.example.com", "admin-token")
//	run, err := c.CreateRun(ctx, client.CreateRunRequest{
//	    Agent: "claude-code",
//	    Repo:  "org/repo",
//	    Task:  "fix issue #42",
//	})
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Client is the Wardyn SDK client. Construct it with New or by filling the
// fields directly. BaseURL and Token are required; HTTPClient defaults to
// http.DefaultClient when nil.
//
// All methods accept a context; the context controls cancellation and deadline
// for the underlying HTTP call.
type Client struct {
	// BaseURL is the wardynd root, e.g. "https://wardyn.example.com".
	// A trailing slash is stripped automatically.
	BaseURL string

	// Token is the admin bearer token configured in wardynd (AdminToken).
	Token string

	// HTTPClient, when non-nil, is used instead of http.DefaultClient.
	HTTPClient *http.Client

	// Principal, when non-empty, is sent as the X-Wardyn-Principal header.
	// This overrides the server-side principal attribution for multi-user dev;
	// in production the token's subject is used instead.
	Principal string
}

// New returns a Client configured with baseURL and token.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token}
}

// APIError is returned when the server responds with a non-2xx status code.
// Status is the HTTP status code; Body is the raw response body (trimmed to
// 2 KiB) for diagnostic display. Callers may use errors.As to extract it.
type APIError struct {
	// Status is the HTTP status code, e.g. 404.
	Status int
	// Body is the raw server response body (capped at 2048 bytes).
	Body string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("wardyn: API error %d: %s", e.Status, e.Body)
}

// CreateRunRequest is the body for POST /api/v1/runs.
type CreateRunRequest struct {
	Agent    string     `json:"agent"`
	Repo     string     `json:"repo"`
	Task     string     `json:"task,omitempty"`
	PolicyID *uuid.UUID `json:"policy_id,omitempty"`
	// ConfinementClass, when set, requests a specific confinement class
	// ("CC1"/"CC2"/"CC3"). Empty inherits the policy minimum; an unknown
	// non-empty value is rejected by the server with 400.
	ConfinementClass string `json:"confinement_class,omitempty"`
	// Interactive requests an interactive run: the sandbox comes up idle (no
	// agent task is exec'd) so a human can attach to it (wardyn attach <id>).
	// Pair with a never-reap policy (AutoStopAfterSec < 0) or the idle reaper
	// will stop the idle sandbox. Task is ignored for an interactive run.
	Interactive bool `json:"interactive,omitempty"`
	// InlinePolicy, when set, supplies the run's full RunPolicySpec INLINE
	// instead of referencing a stored PolicyID. It is MUTUALLY EXCLUSIVE with
	// PolicyID (the server rejects both with 400); neither set falls back to the
	// configured default. The server validates it exactly like a stored policy
	// (mounts pass the same deny-list; api_key grants must reference an existing
	// secret) and attaches it with no stored policy id.
	InlinePolicy *RunPolicySpec `json:"inline_policy,omitempty"`
	// DevcontainerRepo, when set AND an image builder is wired (WARDYN_ENVBUILD),
	// triggers a devcontainer build of that git repo whose resulting image becomes
	// the sandbox image. Ignored (degrades to the convention image) when no builder
	// is wired. Mutually exclusive with Image.
	DevcontainerRepo string `json:"devcontainer_repo,omitempty"`
	// DevcontainerRef is the optional git ref (branch/tag/sha) to build for
	// DevcontainerRepo.
	DevcontainerRef string `json:"devcontainer_ref,omitempty"`
	// Image, when set, is a USER-supplied base image (Bring Your Own Image); the
	// server wraps it with the runner tools via a trusted finalize stage and
	// requires an image builder to be wired (WARDYN_ENVBUILD) — an explicit
	// Image with no builder wired is a hard 400 rather than a silent fallback.
	// Mutually exclusive with DevcontainerRepo.
	Image string `json:"image,omitempty"`
	// TaskMode selects how a non-interactive run executes Task: "" / "harness"
	// (default) runs the agent harness; "exec" runs Task as a plain shell
	// command in the same governed sandbox (no agent, no LLM credentials — the
	// BYOA/CI lane; see docs/CI.md). Ignored for an interactive run.
	TaskMode string `json:"task_mode,omitempty"`
	// ComposeSessionID correlates a run launched from the AI Run Composer back
	// to the compose conversation that produced it. It is stamped into the
	// run.create audit event, so filtering the audit feed on it reconstructs the
	// whole compose→launch trail. Purely a correlation label: it grants nothing
	// and is not validated server-side.
	ComposeSessionID string `json:"compose_session_id,omitempty"`
}

// CreateRun submits a new agent run to the control plane.
// Returns the created AgentRun (state PENDING or RUNNING) on success.
// Status 201 on success; 400 on validation failure; 422 on policy/confinement
// mismatch; 503 when the runner is unavailable.
func (c *Client) CreateRun(ctx context.Context, req CreateRunRequest) (types.AgentRun, error) {
	var out types.AgentRun
	err := c.do(ctx, http.MethodPost, "/api/v1/runs", req, &out)
	return out, err
}

// GetRun fetches a single AgentRun by its UUID.
// Returns 404/APIError when the run does not exist.
func (c *Client) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	var out types.AgentRun
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+id.String(), nil, &out)
	return out, err
}

// ListRuns returns all runs in reverse creation order.
func (c *Client) ListRuns(ctx context.Context) ([]types.AgentRun, error) {
	var out []types.AgentRun
	err := c.do(ctx, http.MethodGet, "/api/v1/runs", nil, &out)
	return out, err
}

// ListGrants returns the credential-grant eligibility records for a run.
// These are eligibility records (what the run MAY request), not issued
// credentials — some may never be minted.
// Returns 404/APIError when the run does not exist.
func (c *Client) ListGrants(ctx context.Context, runID uuid.UUID) ([]types.CredentialGrant, error) {
	var out []types.CredentialGrant
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID.String()+"/grants", nil, &out)
	return out, err
}

// KillRunResponse is the body returned by POST /api/v1/runs/{id}/kill.
type KillRunResponse struct {
	ID    uuid.UUID      `json:"id"`
	State types.RunState `json:"state"`
}

// KillRun initiates the kill sequence for a run: sandbox teardown, identity
// revocation, credential revocation, then state transition to KILLED.
// Returns 202/Accepted with the final state on success.
// Returns 404/APIError when the run does not exist.
func (c *Client) KillRun(ctx context.Context, id uuid.UUID) (KillRunResponse, error) {
	var out KillRunResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+id.String()+"/kill", nil, &out)
	return out, err
}

// ListApprovals returns approval requests filtered by state.
// Pass an empty string to return all states.
// Valid states: "PENDING", "APPROVED", "DENIED", "EXPIRED" (types.ApprovalState).
func (c *Client) ListApprovals(ctx context.Context, state types.ApprovalState) ([]types.ApprovalRequest, error) {
	path := "/api/v1/approvals"
	if state != "" {
		path += "?state=" + url.QueryEscape(string(state))
	}
	var out []types.ApprovalRequest
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// approvalDecisionRequest is the shared approve/deny body.
type approvalDecisionRequest struct {
	Reason string `json:"reason,omitempty"`
}

// Approve transitions an approval request to APPROVED.
// reason is optional; pass an empty string to omit it.
// Returns 409/APIError when the approval has already been decided.
// Returns 404/APIError when the approval does not exist.
func (c *Client) Approve(ctx context.Context, id uuid.UUID, reason string) (types.ApprovalRequest, error) {
	var out types.ApprovalRequest
	body := approvalDecisionRequest{Reason: reason}
	err := c.do(ctx, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", body, &out)
	return out, err
}

// Deny transitions an approval request to DENIED (fail closed).
// reason is optional; pass an empty string to omit it.
// Returns 409/APIError when the approval has already been decided.
// Returns 404/APIError when the approval does not exist.
func (c *Client) Deny(ctx context.Context, id uuid.UUID, reason string) (types.ApprovalRequest, error) {
	var out types.ApprovalRequest
	body := approvalDecisionRequest{Reason: reason}
	err := c.do(ctx, http.MethodPost, "/api/v1/approvals/"+id.String()+"/deny", body, &out)
	return out, err
}

// PolicyRequest is the body for POST/PUT /api/v1/policies. Name is required;
// Spec is validated server-side before persistence (a bad spec is rejected
// with 400, fail closed).
type PolicyRequest struct {
	Name string              `json:"name"`
	Spec types.RunPolicySpec `json:"spec"`
}

// ListPolicies returns all run policies in reverse creation order.
func (c *Client) ListPolicies(ctx context.Context) ([]types.RunPolicy, error) {
	var out []types.RunPolicy
	err := c.do(ctx, http.MethodGet, "/api/v1/policies", nil, &out)
	return out, err
}

// GetPolicy fetches a single RunPolicy by its UUID.
// Returns 404/APIError when the policy does not exist.
func (c *Client) GetPolicy(ctx context.Context, id uuid.UUID) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodGet, "/api/v1/policies/"+id.String(), nil, &out)
	return out, err
}

// CreatePolicy validates and persists a new policy.
// Returns the created RunPolicy (status 201) on success; 400 on an invalid
// name or spec.
func (c *Client) CreatePolicy(ctx context.Context, req PolicyRequest) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodPost, "/api/v1/policies", req, &out)
	return out, err
}

// UpdatePolicy validates and replaces an existing policy's name and spec.
// Returns the updated RunPolicy on success; 404 when unknown; 400 when invalid.
func (c *Client) UpdatePolicy(ctx context.Context, id uuid.UUID, req PolicyRequest) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodPut, "/api/v1/policies/"+id.String(), req, &out)
	return out, err
}

// DeletePolicy removes a policy by id.
// Returns nil on success (204); 404/APIError when the policy does not exist.
func (c *Client) DeletePolicy(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/policies/"+id.String(), nil, nil)
}

// AuditEvents returns the append-only audit trail for the specified run.
// run_id is required by the server; a zero UUID will be rejected with 400.
func (c *Client) AuditEvents(ctx context.Context, runID uuid.UUID) ([]types.AuditEvent, error) {
	path := "/api/v1/audit?run_id=" + url.QueryEscape(runID.String())
	var out []types.AuditEvent
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

// ListSecrets returns the managed secret NAMES (never values). Reserved
// platform-internal keys are excluded server-side. GET /api/v1/secrets, which
// responds {"names":[...]}.
func (c *Client) ListSecrets(ctx context.Context) ([]string, error) {
	var out struct {
		Names []string `json:"names"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/secrets", nil, &out); err != nil {
		return nil, err
	}
	return out.Names, nil
}

// SetSecret stores (or overwrites) a named secret. The value is write-only — no
// API path ever returns it. PUT /api/v1/secrets/{name} with body {"value":...}.
// Returns 400 on an invalid name, 403 for a reserved platform-internal name.
func (c *Client) SetSecret(ctx context.Context, name, value string) error {
	path := "/api/v1/secrets/" + url.PathEscape(name)
	return c.do(ctx, http.MethodPut, path, map[string]string{"value": value}, nil)
}

// DeleteSecret removes a named secret. DELETE /api/v1/secrets/{name}.
// Returns 403 for a reserved platform-internal name.
func (c *Client) DeleteSecret(ctx context.Context, name string) error {
	path := "/api/v1/secrets/" + url.PathEscape(name)
	return c.do(ctx, http.MethodDelete, path, nil, nil)
}

// do executes one HTTP request against the control plane.
//
// body (if non-nil) is JSON-encoded as the request body.
// out (if non-nil) is JSON-decoded from a 2xx response body.
// Any non-2xx response is returned as *APIError.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	// Build the URL: strip trailing slash from BaseURL, then append path.
	base := c.BaseURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	rawURL := base + path

	// Encode the request body.
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("wardyn: marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, rawURL, reqBody)
	if err != nil {
		return fmt.Errorf("wardyn: build request: %w", err)
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Omit rather than send a bare "Bearer " for an empty token: a LOCAL HOST
	// MODE wardynd bypasses public-API auth on a loopback bind (no token
	// needed), and an auth-gated server still returns a clean 401 either way.
	// Matches cmd/wardyn/client.go's apiClient.
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	req.Header.Set("Accept", "application/json")
	if c.Principal != "" {
		req.Header.Set("X-Wardyn-Principal", c.Principal)
	}

	hc := c.HTTPClient
	if hc == nil {
		hc = http.DefaultClient
	}

	resp, err := hc.Do(req)
	if err != nil {
		return fmt.Errorf("wardyn: http: %w", err)
	}
	defer resp.Body.Close()

	// Error path: cap the body at 2 KiB for diagnostic display so a hostile or
	// runaway server cannot exhaust memory via an error response. The cap is
	// for ERROR bodies ONLY — applying it to success bodies truncated any
	// response > 2 KiB and broke JSON decoding (the original finding).
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		const maxErrBody = 2048
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return &APIError{Status: resp.StatusCode, Body: string(raw)}
	}

	// Success path: decode the FULL body (no 2 KiB cap). Streaming via
	// json.NewDecoder avoids buffering the whole body up front; an empty body
	// (e.g. 204) leaves out untouched and returns nil. io.EOF is the normal
	// "no body" signal and is not an error here.
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("wardyn: decode response: %w", err)
		}
	}
	return nil
}
