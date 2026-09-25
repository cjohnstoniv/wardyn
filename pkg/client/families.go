// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
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
// persisted value plus the write's two advisory signals: danglingSecretRefs —
// the names of any secret the document now references that the secret store
// doesn't currently hold (e.g. a `site-config apply` recovery run before the
// referenced secrets were restored) — and onboardingMarkIgnored, true when the
// body named an onboarding_completed_at the server did not keep (that mark is
// server-owned and always carried forward; see types.SiteConfig). Both are
// advisory only, never an error: the ref is still saved as given, and the write
// still succeeded. PUT /api/v1/site-config.
//
// Integrations is stripped from cfg before the request: the server rejects a
// non-empty one outright (integrations are managed through their own
// endpoints, never PUT /site-config), so the documented disaster-recovery
// round-trip — `wardyn site-config get > f` before a reset, `wardyn
// site-config apply f` after — 400ed outright the moment any integration was
// ever stored (PLATFORM-API-5). Stripped here, once, so no caller has to
// remember to (mirrors ui/src/app/lib/api/health.ts's identical fix on the
// TS side).
func (c *Client) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (
	out types.SiteConfig, danglingSecretRefs []string, onboardingMarkIgnored bool, err error,
) {
	cfg.Integrations = nil
	var resp struct {
		types.SiteConfig
		DanglingSecretRefs           []string `json:"dangling_secret_refs,omitempty"`
		OnboardingCompletedAtIgnored bool     `json:"onboarding_completed_at_ignored,omitempty"`
	}
	err = c.do(ctx, http.MethodPut, "/api/v1/site-config", cfg, &resp)
	return resp.SiteConfig, resp.DanglingSecretRefs, resp.OnboardingCompletedAtIgnored, err
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

// DeviceEnrolmentTokenRequest is the POST /api/v1/admin/devices/enrolment-tokens
// body. internal/api aliases it (mintEnrolmentTokenRequest), so the strict
// server decode and this struct are one type.
type DeviceEnrolmentTokenRequest struct {
	// Name is the laptop's inventory name; the device it enrols carries it.
	Name string `json:"name"`
}

// MintDeviceEnrolmentToken mints a single-use token one laptop's first boot
// exchanges for its device credential (admin only). The returned Token is the
// only copy — the server keeps a hash — and it expires unused after 72 hours.
// POST /api/v1/admin/devices/enrolment-tokens.
func (c *Client) MintDeviceEnrolmentToken(ctx context.Context, name string) (DeviceEnrolmentToken, error) {
	var out DeviceEnrolmentToken
	err := c.do(ctx, http.MethodPost, "/api/v1/admin/devices/enrolment-tokens", DeviceEnrolmentTokenRequest{Name: name}, &out)
	return out, err
}

// ListDevices returns every enrolled device, revoked ones included, newest
// first (admin or security_admin). GET /api/v1/admin/devices.
func (c *Client) ListDevices(ctx context.Context) ([]Device, error) {
	var out []Device
	err := c.do(ctx, http.MethodGet, "/api/v1/admin/devices", nil, &out)
	return out, err
}

// RevokeDevice cuts one device off: its next audit push or heartbeat answers
// 401 (admin or security_admin). 404 for an unknown or already-revoked id.
// DELETE /api/v1/admin/devices/{id}.
func (c *Client) RevokeDevice(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/admin/devices/"+id.String(), nil, nil)
}

// ListSSHKeysPage is ListSSHKeys plus the server's X-Wardyn-Truncated signal:
// truncated=true means a further page exists and this one is not the whole
// list. See client.go's package doc's "# Pagination".
func (c *Client) ListSSHKeysPage(ctx context.Context, opts ...ListOpts) (keys []types.SSHPublicKey, truncated bool, err error) {
	var hdr http.Header
	err = c.do(ctx, http.MethodGet, appendListOpts("/api/v1/me/ssh-keys", opts), nil, &keys, &hdr)
	return keys, hdr.Get("X-Wardyn-Truncated") == "true", err
}

// ListDeviceEnrolmentTokens returns every enrolment token still redeemable —
// minted, not yet redeemed, revoked or expired — newest first, never the token
// itself (admin or security_admin). GET /api/v1/admin/devices/enrolment-tokens.
func (c *Client) ListDeviceEnrolmentTokens(ctx context.Context) ([]DeviceEnrolmentToken, error) {
	var out []DeviceEnrolmentToken
	err := c.do(ctx, http.MethodGet, "/api/v1/admin/devices/enrolment-tokens", nil, &out)
	return out, err
}

// RevokeDeviceEnrolmentToken cancels a token that has not been redeemed yet, so
// no laptop can trade it for a device credential (admin or security_admin). 404
// for one already redeemed, revoked, expired or unknown.
// DELETE /api/v1/admin/devices/enrolment-tokens/{id}.
func (c *Client) RevokeDeviceEnrolmentToken(ctx context.Context, id uuid.UUID) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/admin/devices/enrolment-tokens/"+id.String(), nil, nil)
}

// ListSSHKeys returns the caller's own registered SSH gateway keys — the
// gateway's entire trust root (docs/SSH.md §1). There is no admin view of
// another principal's keys. Pass a ListOpts to page; prefer ListSSHKeysPage,
// which also returns the server's truncation signal. GET /api/v1/me/ssh-keys.
func (c *Client) ListSSHKeys(ctx context.Context, opts ...ListOpts) ([]types.SSHPublicKey, error) {
	keys, _, err := c.ListSSHKeysPage(ctx, opts...)
	return keys, err
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

// ── Drives (migration 0054) ─────────────────────────────────────────────────

// DriveRequest is the POST /drives / PUT /drives/{id} body — one
// admin-registered drive. ID/CreatedAt/UpdatedAt/CreatedBy are never accepted
// from the wire: the id comes from the path (an update) or the server (a
// create), and provenance is always server-assigned. Name and Backend are
// always on the wire (no omitempty); the rest default to the zero value a new
// drive would otherwise want.
type DriveRequest struct {
	Name    string       `json:"name"`
	Backend DriveBackend `json:"backend"`
	// HostRoot is the operator-mounted tree a host_path drive binds a
	// subdirectory of. Meaningless for every other backend, and refused
	// outright when it names a path outside this deployment's configured
	// drive host roots.
	HostRoot string `json:"host_root,omitempty"`
	// StorageClass is the k8s_pvc provisioner to request; "" means the
	// cluster default. Meaningless for every other backend.
	StorageClass string `json:"storage_class,omitempty"`
	// HomeTemplate is which identity a drive's per-user home name is derived
	// from. "" means the server's default (HomeTemplateHash).
	HomeTemplate HomeTemplate `json:"home_template,omitempty"`
	// SizeMiB is the allocation, not a guarantee — see types.StorageEnforcement.
	// Required (and enforced) on a k8s_pvc drive; advisory elsewhere.
	SizeMiB int `json:"size_mib,omitempty"`
	// Writable is the drive's DEFAULT posture and it defaults to false. A
	// grant may narrow it and a run may narrow it again; neither may widen it.
	Writable bool `json:"writable,omitempty"`
	// Reclaim is the declared intent for the drive's storage objects once an
	// allocation is removed. "" means the server's default (DriveReclaimRetain).
	Reclaim DriveReclaim `json:"reclaim,omitempty"`
}

// DriveGrantRequest is the POST /drives/grants body — one allocation of a
// drive to a subject, upserted by the natural key (subject_type, subject): a
// second POST for the same subject repoints its single row rather than
// accumulating another one.
type DriveGrantRequest struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	DriveID     uuid.UUID             `json:"drive_id"`
	// Priority breaks ties within a tier (higher wins) when a subject's
	// membership qualifies it for more than one group-tier grant. It does not
	// cross tiers (user beats group beats all).
	Priority int `json:"priority"`
	// SizeMiBOverride replaces the drive's allocation for this subject. 0
	// means "use the drive's".
	SizeMiBOverride int `json:"size_mib_override,omitempty"`
	// WritableOverride is TRI-STATE: nil is "use the drive's posture", and an
	// explicit false is "this subject reads only" even on a writable drive.
	WritableOverride *bool `json:"writable_override,omitempty"`
	// HomeOverride is TRI-STATE for the same reason: nil leaves a previously
	// pinned directory name alone, and an explicit "" (or a name) states the
	// field, which is itself the confirmation to change it. User-tier
	// subjects only.
	HomeOverride *string `json:"home_override,omitempty"`
	// Enabled is TRI-STATE and defaults to true when nil: allocating a drive
	// is a deliberate act, so omitting the field means "give it to them", not
	// "pause it". An explicit false pauses the allocation without deleting it.
	Enabled *bool `json:"enabled,omitempty"`
}

// DrivesDocument is GET /drives's body and ApplyDrives's parameter: every
// registered drive, every allocation (GetDrives reads every page), and the
// deployment facts an operator cannot derive from the rows alone (whether
// host_path drives are even authorable here, which substrate this deployment
// dispatches to, and whether the org switch is off). `wardyn drive get` prints this verbatim;
// ApplyDrives strict-decodes it back — HostRootsConfigured, RunnerTarget,
// Disabled and GrantTotal are read-only and ignored on write.
type DrivesDocument struct {
	Drives              []UserDriveListItem `json:"drives"`
	Grants              []UserDriveGrant    `json:"grants"`
	HostRootsConfigured bool                `json:"host_roots_configured"`
	RunnerTarget        string              `json:"runner_target"`
	Disabled            bool                `json:"disabled"`
	GrantTotal          int                 `json:"grant_total"`
}

// GetDrives returns every registered drive and EVERY allocation. GET
// /api/v1/drives serves allocations one page at a time (X-Wardyn-Truncated),
// so this reads ?offset= until the flag clears, and then refuses a result
// whose allocation count is not the server's grant_total: a document that
// silently dropped allocations would, fed back to ApplyDrives, restore a
// partial set.
func (c *Client) GetDrives(ctx context.Context) (DrivesDocument, error) {
	grants := []UserDriveGrant{}
	for {
		var page DrivesDocument
		var hdr http.Header
		path := appendListOpts("/api/v1/drives", []ListOpts{{Offset: len(grants)}})
		if err := c.do(ctx, http.MethodGet, path, nil, &page, &hdr); err != nil {
			return DrivesDocument{}, err
		}
		grants = append(grants, page.Grants...)
		more := hdr.Get("X-Wardyn-Truncated") == "true"
		// Past grant_total, or no progress, is a server not paging the way this
		// loop reads it; stopping there is what bounds the loop.
		if len(grants) > page.GrantTotal || more && len(page.Grants) == 0 {
			return DrivesDocument{}, fmt.Errorf("drives: the server's allocation pages do not add up (%d read, %d reported); retry",
				len(grants), page.GrantTotal)
		}
		if !more {
			if len(grants) != page.GrantTotal {
				return DrivesDocument{}, fmt.Errorf("drives: read %d allocations but the server reports %d; allocations changed while being read, retry",
					len(grants), page.GrantTotal)
			}
			page.Grants = grants
			return page, nil
		}
	}
}

// ApplyDrives upserts every drive and grant doc names, over the existing
// POST /drives, PUT /drives/{id} and POST /drives/grants routes — there is no
// bulk-write route, and none is added. A drive is routed by the id doc
// carries: one already issued by GetDrives (non-nil) is REPLACED in place
// (PUT), a zero id is CREATED (POST) — which is what makes `wardyn drive get
// > f && wardyn drive apply f` a no-op: the ids `get` wrote back are exactly
// what route the re-`apply` to an update of the same rows, not a second copy
// under a fresh name. A grant carries no id of its own; every one is POSTed,
// and the server's own (subject_type, subject) upsert repoints an existing
// allocation rather than duplicating it.
//
// Nothing doc omits is touched and nothing is deleted — neither POST nor PUT
// can express that, and this function does not attempt it by other means.
// Every write's saved row replaces the caller's copy in place, so a partial
// failure (returned as the second value) leaves doc's earlier entries holding
// what was actually persisted. On success, the returned document is a fresh
// GetDrives — the authoritative post-write state, including the derived
// fields (grant counts, deployment facts) a write response cannot carry.
func (c *Client) ApplyDrives(ctx context.Context, doc DrivesDocument) (DrivesDocument, error) {
	for i, d := range doc.Drives {
		req := DriveRequest{
			Name: d.Name, Backend: d.Backend, HostRoot: d.HostRoot,
			StorageClass: d.StorageClass, HomeTemplate: d.HomeTemplate,
			SizeMiB: d.SizeMiB, Writable: d.Writable, Reclaim: d.Reclaim,
		}
		var saved UserDrive
		var err error
		if d.ID == uuid.Nil {
			err = c.do(ctx, http.MethodPost, "/api/v1/drives", req, &saved)
		} else {
			err = c.do(ctx, http.MethodPut, "/api/v1/drives/"+d.ID.String(), req, &saved)
		}
		if err != nil {
			return DrivesDocument{}, fmt.Errorf("apply drive %q: %w", d.Name, err)
		}
		doc.Drives[i].UserDrive = saved
	}
	for i, g := range doc.Grants {
		req := DriveGrantRequest{
			SubjectType: g.SubjectType, Subject: g.Subject, DriveID: g.DriveID,
			Priority: g.Priority, SizeMiBOverride: g.SizeMiBOverride,
			WritableOverride: g.WritableOverride,
			// Always stated, never nil: HomeOverride and Enabled are the two
			// fields the server treats "the client said nothing" as
			// meaningfully different from "the client said this exact value"
			// (see their doc comments on DriveGrantRequest) — a round trip
			// that left either nil would either fail to restate a pinned
			// home name (409, ErrConflict) or, worse, mean something
			// different from what GetDrives just reported.
			HomeOverride: &g.HomeOverride,
			Enabled:      &g.Enabled,
		}
		var saved UserDriveGrant
		if err := c.do(ctx, http.MethodPost, "/api/v1/drives/grants", req, &saved); err != nil {
			return DrivesDocument{}, fmt.Errorf("apply drive grant (%s %q on drive %s): %w", g.SubjectType, g.Subject, g.DriveID, err)
		}
		doc.Grants[i] = saved
	}
	return c.GetDrives(ctx)
}
