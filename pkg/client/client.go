// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package client is the public Go SDK for the Wardyn control plane. It uses
// internal/types as the shared JSON source of truth and adds exactly one
// non-stdlib dependency (github.com/google/uuid, for typed ids) so it can be
// embedded in external tooling without friction. The
// in-repo `wardyn` CLI uses THIS client directly (there is no second transport);
// it maps APIError to process exit codes and prints APIError.Error(), which
// unwraps the server's {"error":...} envelope into a human-readable message.
//
// # Coverage
//
// The SDK covers these wardynd public route families (the ones external tooling
// automates); it is a curated subset, NOT a 1:1 mirror of every route:
//
//   - runs (/api/v1/runs):               CreateRun, Preflight, GetRun, ListRuns, ListRunsPage,
//     ListGrants, ListGrantsPage, KillRun, SynthesizeProfile, GetRecording, RunFiles, RunEvents, RunOutput,
//     GetRunPolicy
//   - approvals (/api/v1/approvals):     ListApprovals, ListApprovalsPage, Approve, Deny
//   - policies (/api/v1/policies):       CreatePolicy, GetPolicy, GetDefaultPolicy, ListPolicies,
//     ListPoliciesPage, UpdatePolicy, DeletePolicy
//   - workspaces (/api/v1/workspaces):   CreateWorkspace, GetWorkspace, ListWorkspaces,
//     ListWorkspacesPage, UpdateWorkspace, DeleteWorkspace, ScanWorkspace, RecordWorkspaceTask
//   - sources (/api/v1/sources):         ListSources, CreateSource, GetSource, ScanSource, DeleteSource
//   - audit (/api/v1/audit):             AuditEvents, AuditEventsPage, RecentAuditEvents,
//     ExportAuditPartition, AuditRetention, DropAuditPartition
//   - secrets (/api/v1/secrets):         ListSecrets, ListSecretsPage, ListSecretsScoped,
//     ListSecretsScopedPage, SetSecret, DeleteSecret
//   - site-config (/api/v1/site-config): GetSiteConfig, PutSiteConfig, PutSiteConfigResult
//   - drives (/api/v1/drives):           GetDrives, ApplyDrives
//   - presets (/api/v1/presets):         ListPresets, GetPreset, PutPreset, DeletePreset, ApplyPresets
//   - governance (/api/v1/governance):   GetGovernance, ApplyGovernance, ApplyGovernanceResult
//   - governance changes (/api/v1/governance/changes): ListGovernanceChanges, GetGovernanceChange,
//     ApproveGovernanceChange, RejectGovernanceChange
//   - setup (/api/v1/setup):             SetupStatus, ConnectManagedSubscription, DisconnectManagedSubscription
//   - identity (/api/v1/me):             Me — and, on the same prefix, ListSSHKeys/
//     ListSSHKeysPage/AddSSHKey/DeleteSSHKey (/api/v1/me/ssh-keys). The rest of
//     /api/v1/me is NOT wrapped: see below.
//   - health (/healthz):                 Healthz
//   - sessions (/api/v1/sessions):       RevokeSessions
//   - devices (/api/v1/admin/devices):   MintDeviceEnrolmentToken, ListDeviceEnrolmentTokens, RevokeDeviceEnrolmentToken, ListDevices, RevokeDevice
//   - people (/api/v1/people):           ListPeople, and ErasePerson (a person's retained records, by scope, 0.8.6); the other writes below stay unwrapped
//
// NOT covered — drive these with the CLI or raw HTTP. This half is a CENSUS of
// every registered route family the SDK does not wrap, not a list of
// highlights: it read "attach, harness-login and /internal/*" while seven whole
// families 0.7 added were missing from BOTH halves, so docs/sdk.md's "the exact
// list of what it wraps and what it does not" was exact about neither.
//
//   - /api/v1/user-types     — the org's user types (0.8)
//   - /api/v1/key-domains    — which declared key domain wraps a person's next principal key (0.8.6). Security tier
//   - /api/v1/approval-notify — each approval notification channel's delivery health (0.8.6).
//     Security tier, and read by the console's Settings card rather than by tooling
//   - /api/v1/permissions    — capability grants and per-kind enforcement (0.7)
//   - /api/v1/access         — directory search and group->role mappings (0.7)
//   - /api/v1/tokens         — admin-tier API tokens (0.7); /api/v1/me/tokens is the
//     self-service half, also unwrapped
//   - /api/v1/people         — the writes: creating a person, erasing their stored credentials (0.8, offboarding)
//   - /api/v1/scim           — the Settings SCIM card's read-only status (0.8.6); the SCIM server itself is /scim/v2
//   - /api/v1/workspace-providers — the org's git-provider policy (allowed base
//     URLs, credential lanes) and storage ceilings (0.7.2). Admin-only, and
//     authored through the console's providers page rather than by tooling
//   - /api/v1/agent-providers — the org's agent roster: which coding agents this
//     deployment offers, each one's model-access lane, and whether that
//     credential is shared or per-person (0.7.2). Admin-only, same page
//   - /api/v1/model-providers — the org's model-provider records (0.8). Admin-only
//   - /api/v1/integrations   — integration definitions (0.7)
//   - /api/v1/base-images    — the base-image library (0.7)
//   - /api/v1/admin          — operator maintenance (the sandbox sweep; devices is wrapped)
//   - /api/v1/devices        — an enrolled laptop's daemon routes (enrol, audit, heartbeat)
//   - /api/v1/internal       — the AGENT-facing plane (mint, decisions, groundtruth,
//     scan-results, token renew). Deliberately unwrapped: it is the sandbox's
//     surface, not an operator's.
//   - /api/v1/me             — beyond Me and ssh-keys: capabilities, run-layout, tokens
//   - /api/v1/auth, /auth/login, /auth/callback — the browser SSO leg, plus the
//     harness-login device flow. A redirect dance, not an API call.
//   - /api/v1/scm            — the per-user Azure DevOps sign-in (0.7.10): a
//     sign-in door and its identity-provider callback. Unwrapped for the same
//     reason the SSO leg above is — it is a browser redirect dance whose whole
//     point is a human at a keyboard consenting, and it binds to a browser
//     session an SDK caller does not have.
//   - /api/v1/model-providers-entra — the per-row Azure Foundry sign-in door (0.8.6):
//     a browser redirect dance whose callback is the /api/v1/scm one above.
//     Unwrapped for the same reason.
//   - the attach lane under /api/v1/runs/{id} — attach, attach/ticket,
//     attach/holder, attach/takeover, resources. A WebSocket and its ticket.
//   - /scim/v2/Users, /scim/v2/Users/{id}, /scim/v2/Groups, /scim/v2/Groups/{id} — the
//     identity provider's SCIM connector (0.8.6): suspend and reactivate a person, and
//     remove one from a group. Authenticated by its own bearer
//     (WARDYN_SCIM_TOKEN), never by an operator's credential, so no SDK caller holds it.
//   - /api/v1/branding       — console branding (#1125): the sign-in page's anonymous
//     read and logo, and the Admin view Branding card's save; a console surface
//   - /metrics, /readyz      — the operator's scrape and readiness probes
//   - the console SPA at /   — static assets
//
// (The AI Run Composer's /runs/compose* used to be listed here; those routes
// were removed in 0.5, not left unwrapped.)
//
// TestClientCoversRouteFamilies pins that every family listed above has a
// method, TestRouteFamiliesCoverEveryMethod pins the reverse, and
// TestSDKCensusNamesEveryRouteFamily (internal/api) pins the COMPLEMENT: every
// route family the router actually registers appears in one of the two halves
// above, so a new one cannot land in neither.
//
// # Pagination
//
// The list endpoints and the audit trail accept an optional ListOpts (variadic,
// so existing zero-arg calls are unchanged) that sends ?limit=&offset=. A page
// may be truncated (the server sets X-Wardyn-Truncated); re-request with Offset
// advanced by len(page) to page forward.
//
// EVERY list family surfaces that signal, through a *Page variant returning it
// as a bool: ListRunsPage, ListApprovalsPage, ListPoliciesPage,
// ListWorkspacesPage, AuditEventsPage, ListGrantsPage, ListSSHKeysPage,
// ListSecretsPage. The plain forms are thin wrappers that
// discard it, so existing callers are unchanged — but they leave the caller to
// infer completeness from len(page) == the limit it happened to pass, a guess
// that silently breaks the moment a caller omits Limit and gets the server's
// own default page size. This doc used to name the header as the pagination
// contract for the list endpoints while only the audit method honoured it: the
// four list families discarded the header, so `wardyn run|approvals|policy|
// workspace list` printed a truncated page with exit 0 and nothing on stderr.
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
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ListOpts carries the server's ?limit=&offset= pagination for the list and
// audit endpoints. The zero value sends nothing, so the server applies its
// default page size. Methods take it variadically; pass at most one.
type ListOpts struct {
	Limit  int
	Offset int
}

// appendListOpts returns path with ?limit=&offset= merged in (respecting an existing
// query string). Zero fields are omitted; an empty opts slice leaves path as-is.
func appendListOpts(path string, opts []ListOpts) string {
	if len(opts) == 0 {
		return path
	}
	q := url.Values{}
	if opts[0].Limit > 0 {
		q.Set("limit", strconv.Itoa(opts[0].Limit))
	}
	if opts[0].Offset > 0 {
		q.Set("offset", strconv.Itoa(opts[0].Offset))
	}
	if len(q) == 0 {
		return path
	}
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	return path + sep + q.Encode()
}

// Client is the Wardyn SDK client. Construct it with New or by filling the
// fields directly. BaseURL and Token are required; HTTPClient defaults to a
// client that never follows a redirect when nil.
//
// All methods accept a context; the context controls cancellation and deadline
// for the underlying HTTP call.
type Client struct {
	// BaseURL is the wardynd root, e.g. "https://wardyn.example.com".
	// A trailing slash is stripped automatically.
	BaseURL string

	// Token is the admin bearer token configured in wardynd (AdminToken).
	Token string

	// HTTPClient, when non-nil, is used instead of the default client, and its
	// redirect policy is YOURS: the default returns a 3xx as an *APIError
	// rather than following it, but a client you supply is used as it is.
	// Following a redirect can replay a request body (a secret value) and the
	// Authorization bearer at wherever Location points, so a caller who sets
	// this should set a CheckRedirect func that returns http.ErrUseLastResponse.
	HTTPClient *http.Client

	// Principal, when non-empty, is sent as the X-Wardyn-Principal header — a
	// DEV-ONLY override for simulating different principals against a local
	// wardynd. The server honors it ONLY in local (no-auth) mode. Under
	// admin-token auth it is ignored and the action is attributed to
	// actor_type=system / principal "admin-token" (the token is an opaque shared
	// bearer, not a JWT — there is no subject to extract); under OIDC the
	// verified subject wins. Use OIDC for real per-human attribution.
	Principal string
}

// New returns a Client configured with baseURL and token.
func New(baseURL, token string) *Client {
	return &Client{BaseURL: baseURL, Token: token}
}

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

// ListApprovals returns approval requests filtered by state and (optionally)
// by run. Pass an empty state to return all states, and uuid.Nil for runID to
// return approvals for every run — the server's own ?run_id= filter
// (internal/api/approvals.go's handleListApprovals) was otherwise unreachable
// from the CLI.
// Valid states: "PENDING", "APPROVED", "DENIED", "EXPIRED", "CANCELLED"
// (types.ApprovalState).
// Prefer ListApprovalsPage, which also returns the server's truncation signal.
func (c *Client) ListApprovals(ctx context.Context, state types.ApprovalState, runID uuid.UUID, opts ...ListOpts) ([]types.ApprovalRequest, error) {
	aps, _, err := c.ListApprovalsPage(ctx, state, runID, opts...)
	return aps, err
}

// ListApprovalsPage is ListApprovals plus the server's X-Wardyn-Truncated
// signal: truncated=true means a further page exists. It matters most on this
// family — an approval queue read as complete when it is not is a pending
// decision nobody sees.
func (c *Client) ListApprovalsPage(ctx context.Context, state types.ApprovalState, runID uuid.UUID, opts ...ListOpts) (aps []types.ApprovalRequest, truncated bool, err error) {
	path := "/api/v1/approvals"
	q := url.Values{}
	if state != "" {
		q.Set("state", string(state))
	}
	if runID != uuid.Nil {
		q.Set("run_id", runID.String())
	}
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts(path, opts), nil, &aps, &hdr)
	return aps, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// approvalDecisionRequest is the shared approve/deny body. Scope/ExpiresAt use
// the server's decision_scope / decision_expires_at wire names exactly — both
// omitempty, so a zero DecisionOpts (or no opts at all) puts neither on the
// wire and the server's ApprovalScope.Normalize() keeps today's run-scoped
// behavior for that caller.
type approvalDecisionRequest struct {
	Reason    string        `json:"reason,omitempty"`
	Scope     ApprovalScope `json:"decision_scope,omitempty"`
	ExpiresAt *time.Time    `json:"decision_expires_at,omitempty"`
}

// DecisionOpts is the optional scope/expiry for Approve/Deny. This repo has no
// WithX functional-option precedent (see ListOpts); DecisionOpts follows the
// same plain-struct-passed-variadically shape. Pass at most one.
type DecisionOpts struct {
	// Scope is the decision's blast radius: ScopeOnce, ScopeRun (the zero
	// value, and today's default), ScopeUntil, or ScopeAlways. Meaningful
	// only for an egress_domain approval; the server rejects it otherwise.
	Scope ApprovalScope
	// Until is the expiry for Scope == ScopeUntil: required by the server in
	// that case, and rejected if set for any other scope.
	Until *time.Time
}

// Approve transitions an approval request to APPROVED.
// reason is optional; pass an empty string to omit it.
// opts is optional (pass at most one); omitting it keeps today's default — a
// run-scoped approval. See DecisionOpts for once/until/always.
// Returns 409/APIError when the approval has already been decided.
// Returns 404/APIError when the approval does not exist.
func (c *Client) Approve(ctx context.Context, id uuid.UUID, reason string, opts ...DecisionOpts) (types.ApprovalRequest, error) {
	var out types.ApprovalRequest
	body := approvalDecisionRequest{Reason: reason}
	if len(opts) > 0 {
		body.Scope, body.ExpiresAt = opts[0].Scope, opts[0].Until
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", body, &out)
	return out, err
}

// Deny transitions an approval request to DENIED (fail closed).
// reason is optional; pass an empty string to omit it.
// opts is optional (pass at most one); omitting it keeps today's default — a
// run-scoped denial. See DecisionOpts for once/until/always.
// Returns 409/APIError when the approval has already been decided.
// Returns 404/APIError when the approval does not exist.
func (c *Client) Deny(ctx context.Context, id uuid.UUID, reason string, opts ...DecisionOpts) (types.ApprovalRequest, error) {
	var out types.ApprovalRequest
	body := approvalDecisionRequest{Reason: reason}
	if len(opts) > 0 {
		body.Scope, body.ExpiresAt = opts[0].Scope, opts[0].Until
	}
	err := c.do(ctx, http.MethodPost, "/api/v1/approvals/"+id.String()+"/deny", body, &out)
	return out, err
}

// AuditFilter narrows an audit query by the server's optional predicates —
// ?since=&until=&action_prefix=&actor_type=&outcome= (see docs/sdk.md's Raw
// HTTP section). The zero value applies no filter. Since/Until are RFC3339
// strings; a malformed one is rejected by the server as a 400, same as the
// raw HTTP API — the client does not duplicate that validation.
type AuditFilter struct {
	Since        string // RFC3339, e.g. time.Now().UTC().Format(time.RFC3339)
	Until        string // RFC3339
	ActionPrefix string
	Actor        string // exact principal, e.g. "alice@corp.example" — "everything X did"
	ActorType    string // "human" | "agent" | "system"
	Outcome      string // "success" | "denied" | "failure"
}

// queryValues renders f as the query params the server's parseAuditFilter
// expects; a zero field is simply omitted (never sent empty).
func (f AuditFilter) queryValues() url.Values {
	q := url.Values{}
	if f.Since != "" {
		q.Set("since", f.Since)
	}
	if f.Until != "" {
		q.Set("until", f.Until)
	}
	if f.ActionPrefix != "" {
		q.Set("action_prefix", f.ActionPrefix)
	}
	if f.Actor != "" {
		q.Set("actor", f.Actor)
	}
	if f.ActorType != "" {
		q.Set("actor_type", f.ActorType)
	}
	if f.Outcome != "" {
		q.Set("outcome", f.Outcome)
	}
	return q
}

// AuditEventsPage is AuditEvents plus the server's X-Wardyn-Truncated signal
// and the optional filter predicates (AuditFilter): truncated=true means this
// page did NOT reach the run's newest event (including run.complete, since
// the per-run trail is chronological/ASC) and the caller must page forward —
// Offset += len(events) — to see the rest. This is the fix for the audit-gap
// where a >1000-event run's newest events could silently drop with no way for
// a caller to even detect it: AuditEvents alone (below) cannot tell "this is
// everything" from "this is page 1 of more".
func (c *Client) AuditEventsPage(ctx context.Context, runID uuid.UUID, filter AuditFilter, opts ...ListOpts) (events []types.AuditEvent, truncated bool, err error) {
	q := filter.queryValues()
	q.Set("run_id", runID.String())
	path := "/api/v1/audit?" + q.Encode()
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts(path, opts), nil, &events, &hdr)
	return events, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// AuditEvents returns the append-only audit trail for the specified run in
// chronological (seq ASC) order, unfiltered. run_id is required by the
// server; a zero UUID is rejected with 400. Pass a ListOpts to page a long
// trail: a truncated page (server sets X-Wardyn-Truncated) is walked forward
// with Offset += len(page), which reaches the terminal run.complete event
// under ASC order — prefer AuditEventsPage, which returns that signal
// directly instead of requiring the caller to infer it.
func (c *Client) AuditEvents(ctx context.Context, runID uuid.UUID, opts ...ListOpts) ([]types.AuditEvent, error) {
	events, _, err := c.AuditEventsPage(ctx, runID, AuditFilter{}, opts...)
	return events, err
}

// RecentAuditEvents returns the newest-first global audit feed (all runs) — the
// SIEM-style tail the Audit view renders. Pass a ListOpts to page.
func (c *Client) RecentAuditEvents(ctx context.Context, opts ...ListOpts) ([]types.AuditEvent, error) {
	var out []types.AuditEvent
	err := c.do(ctx, http.MethodGet, appendListOpts("/api/v1/audit", opts), nil, &out)
	return out, err
}

// ListSecretsPage is ListSecrets plus the server's X-Wardyn-Truncated signal:
// truncated=true means a further page exists and this one is not the whole
// list. See the package doc's "# Pagination". GET /api/v1/secrets, which
// responds {"names":[...],"mine":[...]} — this method surfaces only names,
// same as ListSecrets.
func (c *Client) ListSecretsPage(ctx context.Context, opts ...ListOpts) (names []string, truncated bool, err error) {
	var out struct {
		Names []string `json:"names"`
	}
	var hdr http.Header
	if err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/secrets", opts), nil, &out, &hdr); err != nil {
		return nil, false, err
	}
	return out.Names, hdr.Get("X-Wardyn-Truncated") == "true", nil
}

// ListSecrets returns the managed secret NAMES (never values). Reserved
// platform-internal keys are excluded server-side. Pass a ListOpts to page;
// prefer ListSecretsPage, which also returns the server's truncation signal.
// GET /api/v1/secrets, which responds {"names":[...]}.
func (c *Client) ListSecrets(ctx context.Context, opts ...ListOpts) ([]string, error) {
	names, _, err := c.ListSecretsPage(ctx, opts...)
	return names, err
}

// ListSecretsScopedPage is ListSecretsPage plus the server's `mine` (the
// caller's own namespace): for an operator the two are identical; for a
// member `names` narrows to the operator-owned names an eligible grant
// pairs with, while `mine` is always the caller's own rows. GET
// /api/v1/secrets, which responds {"names":[...],"mine":[...]}.
func (c *Client) ListSecretsScopedPage(ctx context.Context, opts ...ListOpts) (names, mine []string, truncated bool, err error) {
	var out struct {
		Names []string `json:"names"`
		Mine  []string `json:"mine"`
	}
	var hdr http.Header
	if err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/secrets", opts), nil, &out, &hdr); err != nil {
		return nil, nil, false, err
	}
	return out.Names, out.Mine, hdr.Get("X-Wardyn-Truncated") == "true", nil
}

// ListSecretsScoped is ListSecretsScopedPage without the truncation signal.
func (c *Client) ListSecretsScoped(ctx context.Context, opts ...ListOpts) (names, mine []string, err error) {
	names, mine, _, err = c.ListSecretsScopedPage(ctx, opts...)
	return names, mine, err
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

// maxErrBody caps an ERROR response body for diagnostic display so a hostile or
// runaway server cannot exhaust memory via an error response. It is for error
// bodies ONLY — applying it to success bodies truncated any response > 2 KiB and
// broke JSON decoding (the original finding).
const maxErrBody = 2048

// maxPendingErrBody bounds the error body doPending reads: a 409 governance_change_pending carries
// the held change, payload and diff included, which can pass maxErrBody.
const maxPendingErrBody = 1 << 20

// newRequest builds an authenticated request against the control plane. Every
// method routes through it, so the auth/principal headers have ONE owner (the
// raw-stream methods must not hand-copy them).
func (c *Client) newRequest(ctx context.Context, method, path string, body io.Reader, hasBody bool) (*http.Request, error) {
	// Build the URL: strip trailing slash from BaseURL, then append path.
	base := c.BaseURL
	for len(base) > 0 && base[len(base)-1] == '/' {
		base = base[:len(base)-1]
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, body)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	// Omit rather than send a bare "Bearer " for an empty token: a LOCAL HOST
	// MODE wardynd bypasses public-API auth on a loopback bind (no token
	// needed), and an auth-gated server still returns a clean 401 either way.
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.Principal != "" {
		req.Header.Set("X-Wardyn-Principal", c.Principal)
	}
	return req, nil
}

// noRedirect returns the 3xx itself instead of following it. Following would
// turn a write into a GET of wherever Location points (so the call "succeeds"
// against a login page) and replay the body and bearer there.
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

// defaultHTTPClient is used when Client.HTTPClient is nil. It is a package-level
// client of its own: http.DefaultClient is process-global and is never mutated.
var defaultHTTPClient = &http.Client{CheckRedirect: noRedirect}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return defaultHTTPClient
}

// streamTransport bounds everything up to and INCLUDING the response headers
// and nothing after it, so a body may stream for as long as it takes while a
// peer that completes the handshake and then says nothing is still cut off.
// One shared instance: a per-call *http.Transport would leak its own idle
// connection pool.
var streamTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.ResponseHeaderTimeout = 30 * time.Second
	return t
}()

// streamClient is httpClient with the WHOLE-REQUEST deadline shed, for the
// methods that hand back an unread body.
//
// http.Client.Timeout covers reading that body, so the 30s the CLI sets to
// stop a hung JSON poll from wedging `run --wait` (cmd/wardyn/main.go) also
// cut a large recording download off mid-stream — and `run recording -o` then
// left a truncated .cast on disk. A streamed body is bounded by the caller's
// CONTEXT instead; the handshake keeps a deadline of its own via
// streamTransport. A caller who supplied their own Transport keeps it: they
// chose its bounds, and this only drops the one that cannot tell a slow
// download from a hung server.
func (c *Client) streamClient() *http.Client {
	base := c.httpClient()
	if base.Timeout == 0 {
		return base
	}
	cp := *base
	cp.Timeout = 0
	if cp.Transport == nil {
		cp.Transport = streamTransport
	}
	return &cp
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

// do executes one HTTP request against the control plane.
//
// body (if non-nil) is JSON-encoded as the request body.
// out (if non-nil) is JSON-decoded from a 2xx response body.
// Any non-2xx response is returned as *APIError.
// A 202 whose body carries a pending_change (a governance write held for a
// second approver) is returned as *PendingApprovalError and never decoded into
// out: the write has NOT been applied, and out would otherwise read as a
// zero-value success.
// headerOut, if a non-nil *http.Header is passed (at most one — variadic only
// to keep this optional for every existing zero-arg call site), receives the
// raw response header on return, success or error alike — the sole mechanism
// AuditEventsPage uses to surface X-Wardyn-Truncated to a caller.
func (c *Client) do(ctx context.Context, method, path string, body, out any, headerOut ...*http.Header) error {
	pending, err := c.doPending(ctx, method, path, body, out, headerOut...)
	if err != nil {
		return err
	}
	if pending != nil {
		return &PendingApprovalError{Changes: []GovernanceChange{*pending}}
	}
	return nil
}

// doPending is do for a caller that handles a pending change itself: it returns
// the pending change (out untouched) when the server answered 202 with a
// pending_change body, or a 409 governance_change_pending carrying the held
// change, and (nil, nil) for every other success. Detection keys
// on the body's pending_change field, never on the 202 alone: KillRun,
// RecordWorkspaceTask and the scan routes also answer 202 and decode into out.
func (c *Client) doPending(ctx context.Context, method, path string, body, out any, headerOut ...*http.Header) (*GovernanceChange, error) {
	// Encode the request body.
	var reqBody io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := c.newRequest(ctx, method, path, reqBody, body != nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer resp.Body.Close()
	if len(headerOut) > 0 && headerOut[0] != nil {
		*headerOut[0] = resp.Header
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Read past maxErrBody so a held change's body parses whole; Body is capped after.
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, maxPendingErrBody))
		apiErr := NewAPIError(resp.StatusCode, raw)
		if len(apiErr.Body) > maxErrBody {
			apiErr.Body = apiErr.Body[:maxErrBody]
		}
		// A 409 governance_change_pending names the change already holding the target: that is
		// still-pending, the same result the first proposal got, so a repeat apply carries on.
		if resp.StatusCode == http.StatusConflict && apiErr.Reason == "governance_change_pending" {
			var env struct {
				PendingChange *GovernanceChange `json:"pending_change"`
			}
			if json.Unmarshal(raw, &env) == nil && env.PendingChange != nil {
				return env.PendingChange, nil
			}
		}
		return nil, apiErr
	}

	// Success path: decode the FULL body (no 2 KiB cap). Streaming via
	// json.NewDecoder avoids buffering the whole body up front; an empty body
	// (e.g. 204) leaves out untouched and returns nil. io.EOF is the normal
	// "no body" signal and is not an error here.
	var src io.Reader = resp.Body
	if resp.StatusCode == http.StatusAccepted {
		// Only a 202 can be a pending change, and the probe needs the whole
		// body, so this one status is buffered; its bodies are small.
		raw, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("read response: %w", err)
		}
		var probe struct {
			PendingChange json.RawMessage `json:"pending_change"`
		}
		if json.Unmarshal(raw, &probe) == nil && len(probe.PendingChange) > 0 && string(probe.PendingChange) != "null" {
			var change GovernanceChange
			if err := json.Unmarshal(probe.PendingChange, &change); err != nil {
				return nil, fmt.Errorf("decode pending change: %w", err)
			}
			return &change, nil
		}
		src = bytes.NewReader(raw)
	}
	if out != nil {
		if err := json.NewDecoder(src).Decode(out); err != nil && err != io.EOF {
			return nil, fmt.Errorf("decode response: %w", err)
		}
	}
	return nil, nil
}
