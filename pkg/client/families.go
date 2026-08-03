// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// WorkspaceRequest is the body for POST/PUT /api/v1/workspaces. Name is
// required, plus either Sources or the legacy scalar shape (Kind+Source);
// the server validates each source with the same deny-list the run path uses
// (local_dir bind-mount safety, repo slug/URL shape) before persisting.
//
// internal/api aliases this type (`type workspaceRequest = client.WorkspaceRequest`)
// so server and SDK cannot drift; the server rejects unknown JSON fields, so a
// field missing here is unreachable from the SDK rather than merely undocumented.
type WorkspaceRequest struct {
	Name string `json:"name"`
	// Sources is the workspace's composition: one-or-more local_dir/repo/
	// ephemeral sources. Mutually exclusive with the legacy scalar fields
	// below (Kind/Source/Ref/DefaultTarget/Writable) — set one or the other,
	// never both. Omitting both onboards the composition floor: one ephemeral
	// scratch source.
	Sources []types.WorkspaceSource `json:"sources,omitempty"`
	// BaseImage is the workspace's base-image choice. Nil means the platform
	// default convention image for the detected/scanned stack.
	BaseImage *types.WorkspaceBaseImage `json:"base_image,omitempty"`

	// Deprecated: Kind/Source/Ref/DefaultTarget/Writable are the pre-
	// composition-model scalar shape — a single source, folded server-side
	// into Sources[0] (legacyWorkspaceSource). Kept so
	// `wardyn workspace create --kind local_dir --source /x` and existing SDK
	// callers keep working; prefer Sources for a new caller, especially a
	// multi-source one.
	Kind          types.WorkspaceKind `json:"kind,omitempty"`
	Source        string              `json:"source,omitempty"`
	Ref           string              `json:"ref,omitempty"`
	DefaultTarget string              `json:"default_target,omitempty"`
	// Deprecated: see Kind. Writable opts the single legacy source into a
	// READ-WRITE mount for import Record/Verify runs; omitted/false is
	// read-only (the safe default). A sandboxed agent's changes then PERSIST
	// to the host directory. A multi-source caller sets WorkspaceSource.Writable
	// per source instead.
	Writable bool `json:"writable,omitempty"`
	// LLMCred is the operator-owned model/harness credential BINDING for this
	// workspace/container (refs/names only). A run that picks the workspace
	// inherits this model access. Nil => no binding. CREATE-ONLY: the update
	// handler ignores it, so changing a binding needs the standalone
	// PUT /workspaces/{id}/llm-cred route (not covered by this SDK).
	LLMCred *types.WorkspaceLLMCred `json:"llm_cred,omitempty"`
}

// ListWorkspaces returns onboarded workspaces in reverse creation order. Pass a
// ListOpts to page.
func (c *Client) ListWorkspaces(ctx context.Context, opts ...ListOpts) ([]types.Workspace, error) {
	var out []types.Workspace
	err := c.do(ctx, http.MethodGet, appendListOpts("/api/v1/workspaces", opts), nil, &out)
	return out, err
}

// GetWorkspace fetches a single workspace by id. Returns 404/APIError when unknown.
func (c *Client) GetWorkspace(ctx context.Context, id uuid.UUID) (types.Workspace, error) {
	var out types.Workspace
	err := c.do(ctx, http.MethodGet, "/api/v1/workspaces/"+id.String(), nil, &out)
	return out, err
}

// CreateWorkspace onboards a new workspace (status pending_scan). Returns the
// created row (201); 400 on an invalid body/source.
func (c *Client) CreateWorkspace(ctx context.Context, req WorkspaceRequest) (types.Workspace, error) {
	var out types.Workspace
	err := c.do(ctx, http.MethodPost, "/api/v1/workspaces", req, &out)
	return out, err
}

// UpdateWorkspace replaces a workspace's editable identity fields. Returns the
// updated row; 404 when unknown; 400 when invalid.
func (c *Client) UpdateWorkspace(ctx context.Context, id uuid.UUID, req WorkspaceRequest) (types.Workspace, error) {
	var out types.Workspace
	err := c.do(ctx, http.MethodPut, "/api/v1/workspaces/"+id.String(), req, &out)
	return out, err
}

// DeleteWorkspace removes a workspace by id. Returns nil (204); 404 when unknown.
func (c *Client) DeleteWorkspace(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/workspaces/"+id.String(), nil, nil)
}

// ScanWorkspace triggers the workspace scan (populates the least-privilege
// profile). The reply shape varies (a completed profile vs an accepted async
// run), so it is returned as raw JSON. POST /api/v1/workspaces/{id}/scan.
func (c *Client) ScanWorkspace(ctx context.Context, id uuid.UUID) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodPost, "/api/v1/workspaces/"+id.String()+"/scan", nil, &out)
	return out, err
}

// GetSiteConfig returns the operator-wide site config. GET /api/v1/site-config.
func (c *Client) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	var out types.SiteConfig
	err := c.do(ctx, http.MethodGet, "/api/v1/site-config", nil, &out)
	return out, err
}

// PutSiteConfig replaces the operator-wide site config and returns the persisted
// value. PUT /api/v1/site-config.
func (c *Client) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (types.SiteConfig, error) {
	var out types.SiteConfig
	err := c.do(ctx, http.MethodPut, "/api/v1/site-config", cfg, &out)
	return out, err
}

// SetupStatus returns the first-run setup checklist as raw JSON (the response is
// a server-internal struct not exported through internal/types). GET
// /api/v1/setup/status.
func (c *Client) SetupStatus(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/api/v1/setup/status", nil, &out)
	return out, err
}

// harnessCredRequest mirrors the server wire shape for PUT
// /api/v1/setup/harness-credential/{provider} (internal/api/harnesscred.go).
type harnessCredRequest struct {
	Token string `json:"token"`
}

// ConnectManagedSubscription stores a captured provider setup-token so the proxy
// injects it into every eligible run (never resident in the sandbox). The value
// is write-only. PUT /api/v1/setup/harness-credential/{provider}.
func (c *Client) ConnectManagedSubscription(ctx context.Context, provider, token string) error {
	return c.do(ctx, http.MethodPut, "/api/v1/setup/harness-credential/"+provider, harnessCredRequest{Token: token}, nil)
}

// DisconnectManagedSubscription removes a provider's stored managed subscription
// token. DELETE /api/v1/setup/harness-credential/{provider}.
func (c *Client) DisconnectManagedSubscription(ctx context.Context, provider string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/setup/harness-credential/"+provider, nil, nil)
}

// Me returns the caller's resolved identity/attribution as raw JSON. GET
// /api/v1/me.
func (c *Client) Me(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/api/v1/me", nil, &out)
	return out, err
}

// Healthz returns the control-plane health payload as raw JSON. GET /healthz
// (note: NOT under /api/v1, and unauthenticated).
func (c *Client) Healthz(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/healthz", nil, &out)
	return out, err
}
