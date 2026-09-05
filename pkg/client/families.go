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

// ListWorkspacesPage is ListWorkspaces plus the server's X-Wardyn-Truncated
// signal: truncated=true means a further page exists.
func (c *Client) ListWorkspacesPage(ctx context.Context, opts ...ListOpts) (workspaces []types.Workspace, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/workspaces", opts), nil, &workspaces, &hdr)
	return workspaces, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// ListWorkspaces returns onboarded workspaces in reverse creation order. Pass a
// ListOpts to page. Prefer ListWorkspacesPage, which also returns the server's
// truncation signal.
func (c *Client) ListWorkspaces(ctx context.Context, opts ...ListOpts) ([]types.Workspace, error) {
	workspaces, _, err := c.ListWorkspacesPage(ctx, opts...)
	return workspaces, err
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

// ── Sources (tier 1: the shared library) ────────────────────────────────────

// SourceRequest is the body for POST /api/v1/sources — one repo/dir configured
// ONCE (its own requirements contract, its own scan profile) and attached to
// any number of workspaces. The server dedupes on canonical identity
// (kind, locator, ref): re-creating an existing source answers 200 with the
// existing row, contract and all, rather than a duplicate.
type SourceRequest struct {
	Kind types.SourceKind `json:"kind"`
	// Locator is a host directory path (local_dir) or repo slug/clone URL (repo).
	Locator string `json:"locator"`
	// Ref is an optional git ref; repo only. Part of the identity — the same
	// repo at two refs is two sources with two contracts.
	Ref  string `json:"ref,omitempty"`
	Name string `json:"name,omitempty"`
	// Requirements seeds the source's own contract (secret:/egress:/write:
	// keys; integration: keys are tier-3-only and rejected here).
	Requirements map[string]types.WorkspaceRequirement `json:"requirements,omitempty"`
}

// ListSources returns the whole library. GET /api/v1/sources.
func (c *Client) ListSources(ctx context.Context) ([]types.Source, error) {
	var out struct {
		Sources []types.Source `json:"sources"`
	}
	err := c.do(ctx, http.MethodGet, "/api/v1/sources", nil, &out)
	return out.Sources, err
}

// CreateSource upserts a library source by canonical identity (201 new,
// 200 existing row). POST /api/v1/sources.
func (c *Client) CreateSource(ctx context.Context, req SourceRequest) (types.Source, error) {
	var out types.Source
	err := c.do(ctx, http.MethodPost, "/api/v1/sources", req, &out)
	return out, err
}

// GetSource fetches one library source. GET /api/v1/sources/{id}.
func (c *Client) GetSource(ctx context.Context, id uuid.UUID) (types.Source, error) {
	var out types.Source
	err := c.do(ctx, http.MethodGet, "/api/v1/sources/"+id.String(), nil, &out)
	return out, err
}

// ScanSource scans one source — a dir inline (200 with the profile), a repo as
// a governed run (202 with scan_run_id). The reply shape varies, so it is
// returned raw. POST /api/v1/sources/{id}/scan.
func (c *Client) ScanSource(ctx context.Context, id uuid.UUID) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodPost, "/api/v1/sources/"+id.String()+"/scan", nil, &out)
	return out, err
}

// DeleteSource removes a library source. In use → 409 APIError naming the
// attaching workspaces; force detaches them first — those workspaces just stop
// mounting this source, and their next runs succeed without it (there is no
// loud failure at run time: the mount gate has nothing to check for a source
// that used to be there). detachedFrom names whichever workspaces the delete
// actually detached (empty when the source wasn't attached to any), the only
// visibility into what changed. DELETE /api/v1/sources/{id}[?force=1].
func (c *Client) DeleteSource(ctx context.Context, id uuid.UUID, force bool) (detachedFrom []string, err error) {
	path := "/api/v1/sources/" + id.String()
	if force {
		path += "?force=1"
	}
	var out struct {
		DetachedFrom []string `json:"detached_from"`
	}
	if err := c.do(ctx, http.MethodDelete, path, nil, &out); err != nil {
		return nil, err
	}
	return out.DetachedFrom, nil
}

// GetSiteConfig returns the operator-wide site config. GET /api/v1/site-config.
func (c *Client) GetSiteConfig(ctx context.Context) (types.SiteConfig, error) {
	var out types.SiteConfig
	err := c.do(ctx, http.MethodGet, "/api/v1/site-config", nil, &out)
	return out, err
}

// PutSiteConfig replaces the operator-wide site config and returns the
// persisted value plus danglingSecretRefs — the names of any secret the
// document now references that the secret store doesn't currently hold (e.g.
// a `site-config apply` recovery run before the referenced secrets were
// restored). Advisory only, never an error: the ref is still saved as given.
// PUT /api/v1/site-config.
//
// Integrations is stripped from cfg before the request: the server rejects a
// non-empty one outright (integrations are managed through their own
// endpoints, never PUT /site-config), so the documented disaster-recovery
// round-trip — `wardyn site-config get > f` before a reset, `wardyn
// site-config apply f` after — 400ed outright the moment any integration was
// ever stored (PLATFORM-API-5). Stripped here, once, so no caller has to
// remember to (mirrors ui/src/app/lib/api/health.ts's identical fix on the
// TS side).
func (c *Client) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (out types.SiteConfig, danglingSecretRefs []string, err error) {
	cfg.Integrations = nil
	var resp struct {
		types.SiteConfig
		DanglingSecretRefs []string `json:"dangling_secret_refs,omitempty"`
	}
	err = c.do(ctx, http.MethodPut, "/api/v1/site-config", cfg, &resp)
	return resp.SiteConfig, resp.DanglingSecretRefs, err
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

// RevokeSessions is D16's "revoke a human now" admin action:
// POST /api/v1/sessions/revoke, admin-only. Pass exactly one of sub (revoke
// that one principal's sessions) or all=true (revoke every principal's
// sessions) — the server 400s on both or neither. 404s when the deployment
// has no OIDC session store wired (nothing to revoke).
func (c *Client) RevokeSessions(ctx context.Context, sub string, all bool) error {
	return c.do(ctx, http.MethodPost, "/api/v1/sessions/revoke",
		map[string]any{"sub": sub, "all": all}, nil)
}

// ListSSHKeys returns the caller's own registered SSH gateway keys — the
// gateway's entire trust root (docs/SSH.md §1). There is no admin view of
// another principal's keys. GET /api/v1/me/ssh-keys.
func (c *Client) ListSSHKeys(ctx context.Context) ([]types.SSHPublicKey, error) {
	var out []types.SSHPublicKey
	err := c.do(ctx, http.MethodGet, "/api/v1/me/ssh-keys", nil, &out)
	return out, err
}

// AddSSHKey registers one authorized_keys line under the caller's own
// principal and returns the stored record (201). POST /api/v1/me/ssh-keys.
// Fails closed on the server: 422 for private-key material, more than one key,
// or an admin-token caller on an SSO deployment (such a key could never
// authorize a human's run); 409 when the fingerprint is already registered.
func (c *Client) AddSSHKey(ctx context.Context, name, publicKey string) (types.SSHPublicKey, error) {
	var out types.SSHPublicKey
	err := c.do(ctx, http.MethodPost, "/api/v1/me/ssh-keys",
		map[string]string{"name": name, "public_key": publicKey}, &out)
	return out, err
}

// RunFileStat is one changed file in a RunFiles listing.
type RunFileStat struct {
	Path string `json:"path"`
	// Status is git's porcelain code ("M", "A", "D", "??", …); empty when git
	// listed the file in the diff but not in status.
	Status  string `json:"status,omitempty"`
	Added   *int   `json:"added,omitempty"`
	Deleted *int   `json:"deleted,omitempty"`
	// Binary marks a file git declined to count lines for.
	Binary bool `json:"binary,omitempty"`
}

// RunFiles is GET /api/v1/runs/{id}/files: what changed in a RUNNING sandbox's
// workspace, read by one exec inside the sandbox. VCS is "git" (Files is the
// diff-stat), "none" (Path names the directory inspected and it is not a git
// work tree) or "unknown" (git ran and failed — most often it is missing from
// the image). Path is present on every outcome so a caller can tell "no repo
// here" from "looked in the wrong place".
type RunFiles struct {
	VCS       string        `json:"vcs"`
	Path      string        `json:"path,omitempty"`
	Files     []RunFileStat `json:"files"`
	Truncated bool          `json:"truncated"`
}

// RunFiles lists a running sandbox's changed files (owner-or-admin). 409 when
// the run has no sandbox yet, 501 when the runner cannot exec into one.
func (c *Client) RunFiles(ctx context.Context, runID uuid.UUID) (RunFiles, error) {
	var out RunFiles
	err := c.do(ctx, http.MethodGet, "/api/v1/runs/"+runID.String()+"/files", nil, &out)
	return out, err
}
