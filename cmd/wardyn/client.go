// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// apiClient is a thin JSON client for the wardynd public API.
type apiClient struct {
	baseURL string
	token   string
}

// httpClient is shared across every apiClient request — hoisted to package
// scope (rather than built fresh inside do()) so `wardyn run --wait`'s poll
// loop, which can call do() up to ~900 times over its default 30m timeout,
// reuses one connection pool instead of paying a fresh TCP/TLS handshake (and
// discarding a fresh idle-conn pool) on every poll.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// apiError is a typed non-2xx API response. It carries the numeric status so
// exitCodeFor can map it to a process exit code, and its Error() unwraps the
// server's {"error"|"message":...} envelope into a bare human message (while
// KEEPing the "METHOD path: status: msg" prefix).
type apiError struct {
	method     string
	path       string
	statusCode int
	status     string // e.g. "400 Bad Request"
	body       []byte
}

func (e *apiError) Error() string {
	msg := strings.TrimSpace(string(e.body))
	var env struct {
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(e.body, &env) == nil {
		switch {
		case env.Error != "":
			msg = env.Error
		case env.Message != "":
			msg = env.Message
		}
	}
	return fmt.Sprintf("%s %s: %s: %s", e.method, e.path, e.status, msg)
}

func (c *apiClient) do(ctx context.Context, method, path string, body any, out any) error {
	// No token is fine against a LOCAL HOST MODE wardynd (it bypasses public-API
	// auth on a loopback bind). Against an auth-gated server the request simply
	// gets a clear 401 instead of a client-side error.
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encode request: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}
	url := strings.TrimRight(c.baseURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, method, url, reqBody)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("request %s %s: %w", method, path, err)
	}
	defer resp.Body.Close()

	// Error path: cap the body at 2 KiB (matches pkg/client.Client.do) so a
	// hostile or runaway server cannot exhaust CLI memory via an oversized
	// error response.
	if resp.StatusCode >= 400 {
		const maxErrBody = 2048
		data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return &apiError{method: method, path: path, statusCode: resp.StatusCode, status: resp.Status, body: data}
	}
	// Success path: decode the FULL body (no cap) via a streaming decoder; an
	// empty body (e.g. 204) leaves out untouched (io.EOF is the "no body"
	// signal, not an error here).
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil && err != io.EOF {
			return fmt.Errorf("decode response: %w", err)
		}
	}
	return nil
}

// API helper methods mirroring the REST contract.

func (c *apiClient) createRun(ctx context.Context, req createRunBody) (types.AgentRun, error) {
	var run types.AgentRun
	err := c.do(ctx, http.MethodPost, "/api/v1/runs", req, &run)
	return run, err
}

func (c *apiClient) listRuns(ctx context.Context) ([]types.AgentRun, error) {
	var runs []types.AgentRun
	err := c.do(ctx, http.MethodGet, "/api/v1/runs", nil, &runs)
	return runs, err
}

func (c *apiClient) getRun(ctx context.Context, id string) (types.AgentRun, error) {
	var run types.AgentRun
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+id, nil, &run)
	return run, err
}

func (c *apiClient) killRun(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/api/v1/runs/"+id+"/kill", nil, nil)
}

func (c *apiClient) listApprovals(ctx context.Context, state string) ([]types.ApprovalRequest, error) {
	path := "/api/v1/approvals"
	if state != "" {
		path += "?state=" + state
	}
	var out []types.ApprovalRequest
	err := c.do(ctx, http.MethodGet, path, nil, &out)
	return out, err
}

func (c *apiClient) decideApproval(ctx context.Context, id string, approve bool, reason string) (types.ApprovalRequest, error) {
	verb := "deny"
	if approve {
		verb = "approve"
	}
	var out types.ApprovalRequest
	err := c.do(ctx, http.MethodPost, "/api/v1/approvals/"+id+"/"+verb, map[string]string{"reason": reason}, &out)
	return out, err
}

func (c *apiClient) audit(ctx context.Context, runID string) ([]types.AuditEvent, error) {
	var out []types.AuditEvent
	err := c.do(ctx, http.MethodGet, "/api/v1/audit?run_id="+runID, nil, &out)
	return out, err
}

// profileResp is the decoded POST /runs/{id}/profile reply (Recording Mode): the
// synthesized least-privilege sandbox profile plus the observations it was built
// from. Only the fields the CLI renders/saves are modeled.
type profileResp struct {
	Proposed struct {
		InlinePolicy types.RunPolicySpec `json:"inline_policy"`
	} `json:"proposed"`
	OverallRisk  string `json:"overall_risk"`
	Observations struct {
		Domains []struct {
			Host    string   `json:"host"`
			Methods []string `json:"methods"`
		} `json:"domains"`
		Anomalies []string `json:"anomalies"`
	} `json:"observations"`
	Warnings []string `json:"warnings"`
}

func (c *apiClient) synthesizeProfile(ctx context.Context, runID string) (profileResp, error) {
	var out profileResp
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+runID+"/profile", nil, &out)
	return out, err
}

// recordTaskResp is the decoded POST /workspaces/{id}/record reply.
type recordTaskResp struct {
	RecordRunID string   `json:"record_run_id"`
	TaskKey     string   `json:"task_key"`
	Mode        string   `json:"mode"`
	Detail      string   `json:"detail"`
	Warnings    []string `json:"warnings"`
}

func (c *apiClient) recordWorkspaceTask(ctx context.Context, wsID, taskKey string) (recordTaskResp, error) {
	body := map[string]string{"task_key": taskKey}
	var out recordTaskResp
	err := c.do(ctx, http.MethodPost, "/api/v1/workspaces/"+wsID+"/record", body, &out)
	return out, err
}

// createRunBody is the POST /runs body.
type createRunBody struct {
	Agent            string `json:"agent"`
	Repo             string `json:"repo"`
	Task             string `json:"task"`
	PolicyID         string `json:"policy_id,omitempty"`
	ConfinementClass string `json:"confinement_class,omitempty"`
	// Interactive comes up idle (no agent task exec'd) for `wardyn attach`.
	Interactive bool `json:"interactive,omitempty"`
	// InlinePolicy applies a RunPolicySpec directly on the request (from
	// --policy-file). XOR with PolicyID — the server enforces the exclusivity.
	InlinePolicy *types.RunPolicySpec `json:"inline_policy,omitempty"`
	// Image is a USER-supplied base image (Bring Your Own Image); the server
	// wraps it with the runner tools and requires an image builder to be wired.
	Image string `json:"image,omitempty"`
	// TaskMode selects how the sandbox executes the task: "" / "harness" runs
	// the agent harness; "exec" runs the task as a plain shell command.
	TaskMode string `json:"task_mode,omitempty"`
}

// policyBody is the POST/PUT /policies body: a name plus the RunPolicySpec.
type policyBody struct {
	Name string              `json:"name"`
	Spec types.RunPolicySpec `json:"spec"`
}

func (c *apiClient) listPolicies(ctx context.Context) ([]types.RunPolicy, error) {
	var out []types.RunPolicy
	err := c.do(ctx, http.MethodGet, "/api/v1/policies", nil, &out)
	return out, err
}

func (c *apiClient) getPolicy(ctx context.Context, id string) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodGet, "/api/v1/policies/"+id, nil, &out)
	return out, err
}

func (c *apiClient) createPolicy(ctx context.Context, body policyBody) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodPost, "/api/v1/policies", body, &out)
	return out, err
}

func (c *apiClient) updatePolicy(ctx context.Context, id string, body policyBody) (types.RunPolicy, error) {
	var out types.RunPolicy
	err := c.do(ctx, http.MethodPut, "/api/v1/policies/"+id, body, &out)
	return out, err
}

func (c *apiClient) deletePolicy(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/policies/"+id, nil, nil)
}

func (c *apiClient) putSecret(ctx context.Context, name, value string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/secrets/"+name, map[string]string{"value": value}, nil)
}

func (c *apiClient) deleteSecret(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/secrets/"+name, nil, nil)
}

func (c *apiClient) listSecrets(ctx context.Context) ([]string, error) {
	var out struct {
		Names []string `json:"names"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/secrets", nil, &out); err != nil {
		return nil, err
	}
	return out.Names, nil
}
