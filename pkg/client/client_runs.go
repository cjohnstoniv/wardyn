// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GetRun fetches a single AgentRun by its UUID.
// Returns 404/APIError when the run does not exist.
func (c *Client) GetRun(ctx context.Context, id uuid.UUID) (types.AgentRun, error) {
	var out types.AgentRun
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+id.String(), nil, &out)
	return out, err
}

// ListRunsPage is ListRuns plus the server's X-Wardyn-Truncated signal:
// truncated=true means a further page exists and this one is not the whole
// list. See the package doc's "# Pagination" for why every list family answers
// this and not just the audit trail.
func (c *Client) ListRunsPage(ctx context.Context, opts ...ListOpts) (runs []types.AgentRun, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/runs", opts), nil, &runs, &hdr)
	return runs, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// ListRuns returns runs in reverse creation order. Pass a ListOpts to page.
// Prefer ListRunsPage, which also returns the server's truncation signal — this
// form cannot tell "this is everything" from "this is page 1 of more".
func (c *Client) ListRuns(ctx context.Context, opts ...ListOpts) ([]types.AgentRun, error) {
	runs, _, err := c.ListRunsPage(ctx, opts...)
	return runs, err
}

// ListGrantsPage is ListGrants plus the server's X-Wardyn-Truncated signal:
// truncated=true means a further page exists and this one is not the whole
// list. See the package doc's "# Pagination".
// Returns 404/APIError when the run does not exist.
func (c *Client) ListGrantsPage(ctx context.Context, runID uuid.UUID, opts ...ListOpts) (grants []types.CredentialGrant, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/runs/"+runID.String()+"/grants", opts), nil, &grants, &hdr)
	return grants, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// ListGrants returns the credential-grant eligibility records for a run.
// These are eligibility records (what the run MAY request), not issued
// credentials — some may never be minted. Pass a ListOpts to page; prefer
// ListGrantsPage, which also returns the server's truncation signal.
// Returns 404/APIError when the run does not exist.
func (c *Client) ListGrants(ctx context.Context, runID uuid.UUID, opts ...ListOpts) ([]types.CredentialGrant, error) {
	grants, _, err := c.ListGrantsPage(ctx, runID, opts...)
	return grants, err
}

// KillRunResponse is the body returned by POST /api/v1/runs/{id}/kill.
type KillRunResponse struct {
	ID    uuid.UUID      `json:"id"`
	State types.RunState `json:"state"`
}

// KillRun initiates the kill sequence for a run: durable state transition
// (compare-and-swap to KILLED) first, then sandbox teardown, identity
// revocation, and credential revocation.
// Returns 202/Accepted with the final state on success.
// Returns 404/APIError when the run does not exist.
func (c *Client) KillRun(ctx context.Context, id uuid.UUID) (KillRunResponse, error) {
	var out KillRunResponse
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+id.String()+"/kill", nil, &out)
	return out, err
}

// ProfileResult is the decoded POST /api/v1/runs/{id}/profile/synthesize reply (Recording
// Mode): the synthesized least-privilege sandbox profile plus the observations
// it was built from. Only the fields callers render/save are modeled — the full
// server response (profileResponse) additionally carries a per-item risk
// breakdown the SDK does not surface.
type ProfileResult struct {
	Proposed struct {
		InlinePolicy RunPolicySpec `json:"inline_policy"`
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

// SynthesizeProfile runs Recording Mode synthesis for a run: from its already-
// captured audit / egress / ground-truth events the server proposes a tightened,
// reusable RunPolicy ("sandbox profile"). ADVISORY and READ-ONLY — it mints
// nothing and persists no policy (save the proposal via CreatePolicy).
// POST /api/v1/runs/{id}/profile/synthesize (renamed from /profile in 0.8 —
// docs/sdk.md "Renamed in 0.8" — the old path still answers, as a chi alias,
// for one minor). Returns 404 when the run does not exist.
func (c *Client) SynthesizeProfile(ctx context.Context, runID uuid.UUID) (ProfileResult, error) {
	var out ProfileResult
	err := c.do(ctx, http.MethodPost, "/api/v1/runs/"+runID.String()+"/profile/synthesize", nil, &out)
	return out, err
}

// RecordTaskResult is the decoded POST /api/v1/workspaces/{id}/record reply: the
// launched open-egress recording run plus the resolved session key/mode.
type RecordTaskResult struct {
	RecordRunID string   `json:"record_run_id"`
	TaskKey     string   `json:"task_key"`
	Mode        string   `json:"mode"`
	Detail      string   `json:"detail"`
	Warnings    []string `json:"warnings"`
}

// RecordWorkspaceTask launches a named open (allow-all egress) recording session
// for a workspace via the import pipeline — the operator attaches, does the real
// activity, and stops the run to capture what it actually used.
// POST /api/v1/workspaces/{id}/record with body {"task_key": name}. Returns 202
// with the launched run; 503 when no runner is wired; 409 while another import
// step is live.
func (c *Client) RecordWorkspaceTask(ctx context.Context, wsID uuid.UUID, taskKey string) (RecordTaskResult, error) {
	var out RecordTaskResult
	err := c.do(ctx, http.MethodPost, "/api/v1/workspaces/"+wsID.String()+"/record",
		map[string]string{"task_key": taskKey}, &out)
	return out, err
}

// castKeySep separates the run id from a session suffix in a composite cast
// key — a deliberate copy of internal/recording's unexported castSep, not an
// import of it: pkg/client is docs/sdk.md's "one non-stdlib dependency"
// (uuid), and internal/recording (chi, pgx) is a whole-module-graph import
// for two lines of string-joining logic no external consumer should pay for.
const castKeySep = "~"

// castKey mirrors internal/recording.CastKey (see its own doc comment) — same
// rule, kept in sync by hand, not imported (see castKeySep above).
func castKey(runID, suffix string) string {
	if suffix == "" {
		return runID
	}
	return runID + castKeySep + suffix
}

// GetRecording streams a run's terminal recording as raw asciicast bytes (the
// .cast a player consumes). The caller MUST Close the returned reader.
// GET /api/v1/runs/{id}/recording/{key} — the id really does appear twice: the
// route is mounted per-run and its handler takes the recording's own CAST KEY,
// which defaults to the bare run id (a batch run's single recording) when
// session is omitted — existing zero-arg callers are unaffected. An
// INTERACTIVE run can carry multiple recordings, one per attach session, each
// keyed "<runID>~<session>" (castKey above, mirroring internal/recording's own
// doc comment); pass that session id as the optional session argument to fetch
// one of those instead of the run's own bare-id cast. At most one value is
// meaningful; variadic only to keep it optional without a second method name.
// Returns 404/APIError when the run/session has no recording.
func (c *Client) GetRecording(ctx context.Context, runID uuid.UUID, session ...string) (io.ReadCloser, error) {
	suffix := ""
	if len(session) > 0 {
		suffix = session[0]
	}
	key := castKey(runID.String(), suffix)
	path := "/api/v1/runs/" + runID.String() + "/recording/" + url.PathEscape(key)
	req, err := c.newRequest(ctx, http.MethodGet, path, nil, false)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/x-asciicast")
	resp, err := c.streamClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, NewAPIError(resp.StatusCode, raw)
	}
	return resp.Body, nil
}
