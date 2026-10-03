// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"time"

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
// doesn't currently hold (e.g. a `site-config set` recovery run before the
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
// site-config set f` after — 400ed outright the moment any integration was
// ever stored (PLATFORM-API-5). Stripped here, once, so no caller has to
// remember to (mirrors ui/src/app/lib/api/health.ts's identical fix on the
// TS side).
func (c *Client) PutSiteConfig(ctx context.Context, cfg types.SiteConfig) (
	out types.SiteConfig, danglingSecretRefs []string, onboardingMarkIgnored bool, err error,
) {
	res, err := c.PutSiteConfigResult(ctx, cfg)
	return res.SiteConfig, res.DanglingSecretRefs, res.OnboardingCompletedAtIgnored, err
}

// SiteConfigPutResult is PUT /site-config's answer: the stored document plus
// the write's advisory signals. A field is added here rather than a return
// value to PutSiteConfig, whose signature is public.
type SiteConfigPutResult struct {
	types.SiteConfig
	DanglingSecretRefs           []string `json:"dangling_secret_refs,omitempty"`
	OnboardingCompletedAtIgnored bool     `json:"onboarding_completed_at_ignored,omitempty"`
	// BrandingLogoPending (#1215): branding.logo_path was valid but not attached,
	// because the console has no branding record yet. Apply again after saving
	// the Branding card.
	BrandingLogoPending bool `json:"branding_logo_pending,omitempty"`
}

// PutSiteConfigResult is PutSiteConfig with every advisory signal, including
// the ones added after its signature was fixed. PUT /api/v1/site-config.
func (c *Client) PutSiteConfigResult(ctx context.Context, cfg types.SiteConfig) (SiteConfigPutResult, error) {
	cfg.Integrations = nil
	var resp SiteConfigPutResult
	err := c.do(ctx, http.MethodPut, "/api/v1/site-config", cfg, &resp)
	return resp, err
}

// SetupStatus returns the first-run setup checklist as raw JSON (the response is
// a server-internal struct not exported through internal/types). GET
// /api/v1/setup/status.
func (c *Client) SetupStatus(ctx context.Context) (json.RawMessage, error) {
	var out json.RawMessage
	err := c.do(ctx, http.MethodGet, "/api/v1/setup/status", nil, &out)
	return out, err
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

// PeopleListOpts narrows and pages ListPeople. The zero value is the first page of everyone.
type PeopleListOpts struct {
	// Limit is the page size: 50 when zero, at most 200.
	Limit int
	// Cursor is the NextCursor of the previous page.
	Cursor string
	// Query keeps people whose principal or email starts with it.
	Query string
	// State is "active" or "deactivated"; empty keeps both.
	State string
}

// ListPeople returns one page of the people this deployment knows: everyone who has signed in and
// everyone pre-created, each with their role, sign-in state and the counts behind the leaver
// actions (admin or security_admin). Pass a page's NextCursor back as PeopleListOpts.Cursor until
// it is empty. GET /api/v1/people.
func (c *Client) ListPeople(ctx context.Context, opts ...PeopleListOpts) (PersonList, error) {
	q := url.Values{}
	if len(opts) > 0 {
		o := opts[0]
		if o.Limit > 0 {
			q.Set("limit", strconv.Itoa(o.Limit))
		}
		for k, v := range map[string]string{"cursor": o.Cursor, "q": o.Query, "state": o.State} {
			if v != "" {
				q.Set(k, v)
			}
		}
	}
	path := "/api/v1/people"
	if len(q) > 0 {
		path += "?" + q.Encode()
	}
	var out PersonList
	err := c.do(ctx, http.MethodGet, path, nil, &out)
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

// DeleteSSHKey removes one of the caller's own registered SSH gateway keys.
// fp is ssh.FingerprintSHA256's raw form, which routinely contains '/' — it
// is percent-encoded here, matching the server's decode
// (handleDeleteSSHKey). Deleting a fingerprint registered by someone else
// (or one that never existed) 404s the same way — no existence leak across
// principals. DELETE /api/v1/me/ssh-keys/{fingerprint}.
func (c *Client) DeleteSSHKey(ctx context.Context, fp string) error {
	return c.do(ctx, http.MethodDelete, "/api/v1/me/ssh-keys/"+url.PathEscape(fp), nil, nil)
}

// ErasePersonResult is what POST /api/v1/people/{principal}/erasure answers once
// every scope asked for is erased.
type ErasePersonResult struct {
	// Person is the principal the name resolved to.
	Person string `json:"person"`
	// Scopes are the scopes erased, in the order they ran.
	Scopes []string `json:"scopes"`
	// Outcome is each scope's result: "done".
	Outcome map[string]string `json:"outcome"`
	// Detail is each scope's counts.
	Detail map[string]any `json:"detail"`
}

// ErasePerson erases one person's retained records by scope (credentials,
// audit_personal_fields, run_tasks, run_outputs, recordings, mask_copies) in one
// audited act. principal is the person's subject (or an email the deployment
// knows them by) and is percent-encoded here. A scope that fails part way is a
// 500 whose reason is erasure_incomplete: retry with the same scopes. Security
// tier. POST /api/v1/people/{principal}/erasure.
func (c *Client) ErasePerson(ctx context.Context, principal string, scopes []string) (ErasePersonResult, error) {
	var out ErasePersonResult
	err := c.do(ctx, http.MethodPost, "/api/v1/people/"+url.PathEscape(principal)+"/erasure", map[string]any{"scopes": scopes}, &out)
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

// RunOutput is GET /runs/{id}/output's body: the end of a non-interactive run's
// combined stdout/stderr.
type RunOutput struct {
	Output string `json:"output"`
	// Truncated: Output does not start at the run's first byte.
	Truncated bool `json:"truncated"`
	// Complete: the capture is final (a stored row, or a memory tail sealed after
	// the run's last bytes), so a read never gains bytes after it. A run that
	// has just finished is not Complete until then: read again.
	Complete bool `json:"complete"`
	// Source is where the bytes came from; "stdout" for a run's own output.
	Source string `json:"source"`
	// Incomplete: bytes may be missing (a copy did not end in time or failed, or
	// a byte arrived after the capture was sealed).
	Incomplete bool `json:"incomplete"`
	// CaptureGap: the output could not be captured (no process held it), so
	// Output is empty.
	CaptureGap bool `json:"capture_gap"`
	// MaskScope is "run" when the capture was masked against the run's complete
	// manifest throughout, "globals_only" when it was not; empty when this
	// deployment keeps no manifests.
	MaskScope string `json:"mask_scope,omitempty"`
	// CapturedAt is when the final row was written; nil while the run is live.
	CapturedAt *time.Time `json:"captured_at,omitempty"`
}

// RunOutput reads the last tail bytes of a non-interactive run's output
// (owner-or-admin); tail <= 0 asks for all the server keeps (WARDYN_RUN_OUTPUT_TAIL_BYTES, 64 KiB by default). 409 for
// an interactive run, when none is kept, or when it is off; 410 once it has
// expired. Each refusal carries a run_output_* reason.
func (c *Client) RunOutput(ctx context.Context, runID uuid.UUID, tail int) (RunOutput, error) {
	path := "/api/v1/runs/" + runID.String() + "/output"
	if tail > 0 {
		path += "?tail=" + strconv.Itoa(tail)
	}
	var out RunOutput
	err := c.do(ctx, http.MethodGet, path, nil, &out)
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
// > f && wardyn drive set f` a no-op: the ids `get` wrote back are exactly
// what route the re-`set` to an update of the same rows, not a second copy
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

// ── Governance (migration 0052) ─────────────────────────────────────────────

// GovernanceProfileRequest is the POST /governance/profiles / PUT
// /governance/profiles/{id} body — one named ceiling. ID/CreatedAt/UpdatedAt/
// CreatedBy are never accepted from the wire, matching DriveRequest: the id
// comes from the path (an update) or the server (a create), and provenance is
// always server-assigned.
type GovernanceProfileRequest struct {
	Name    string           `json:"name"`
	Ceiling RunPolicySpec    `json:"ceiling"`
	Limits  GovernanceLimits `json:"limits"`
	// Contact is who owns the profile and how a person it refuses asks for a
	// change. nil leaves the stored contact unchanged (an older caller that
	// never sets it cannot wipe it); a pointer to an empty Contact clears it.
	Contact *PolicyContact `json:"contact,omitempty"`
}

// GovernanceProfileResponse is a profile write's body: the saved profile plus
// any omission warnings the write raised (a ceiling grant the deployment no
// longer provisions — see internal/api/governance.go's reintersect note).
type GovernanceProfileResponse struct {
	Profile  GovernanceProfile `json:"profile"`
	Warnings []string          `json:"warnings,omitempty"`
}

// GovernanceAssignmentRequest is POST /governance/assignments's body. There is
// no PUT for an assignment: the natural key (subject_type, subject) upserts,
// repointing an existing binding rather than accumulating a second one, the
// same shape DriveGrantRequest already takes for a drive allocation.
type GovernanceAssignmentRequest struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	ProfileID   uuid.UUID             `json:"profile_id"`
	Priority    int                   `json:"priority"`
}

// GovernanceDocument is GET /governance's body and ApplyGovernance's
// parameter: every profile plus every assignment. `wardyn governance get`
// prints this verbatim; ApplyGovernance strict-decodes it back.
type GovernanceDocument struct {
	Profiles    []GovernanceProfile    `json:"profiles"`
	Assignments []GovernanceAssignment `json:"assignments"`
}

// GetGovernance returns every governance profile and assignment. GET
// /api/v1/governance.
func (c *Client) GetGovernance(ctx context.Context) (GovernanceDocument, error) {
	var out GovernanceDocument
	err := c.do(ctx, http.MethodGet, "/api/v1/governance", nil, &out)
	return out, err
}

// governanceAssignmentKey is the natural key ApplyGovernance and
// handleUpsertGovernanceAssignment both upsert an assignment by.
func governanceAssignmentKey(subjectType CapabilitySubjectType, subject string) string {
	return string(subjectType) + "\x00" + subject
}

// governanceProfileUnchanged reports whether writing p over existing would
// change nothing observable — the check ApplyGovernance runs before every
// profile write so a `get | apply` round trip on an unchanged install issues
// ZERO writes and records ZERO audit rows, rather than re-asserting every
// profile's content on every apply (#1108's stated no-op contract; stricter
// than ApplyDrives, which always re-PUTs/re-POSTs).
func governanceProfileUnchanged(existing, p GovernanceProfile) bool {
	return reflect.DeepEqual(existing.Ceiling, p.Ceiling) && reflect.DeepEqual(existing.Limits, p.Limits) &&
		(p.Contact == nil || reflect.DeepEqual(existing.Contact, p.Contact))
}

// ApplyGovernance upserts every profile and assignment doc names, over the
// existing POST /governance/profiles, PUT /governance/profiles/{id} and POST
// /governance/assignments routes — there is no bulk-write route, and none is
// added.
//
// A PROFILE is routed BY NAME, not by id — the divergence from ApplyDrives,
// and the reason is Name's role server-side: it is the UNIQUE human handle an
// admin actually authors and assigns by (governance.go's own words), while a
// drive's id is what GetDrives/ApplyDrives round-trip on. Upserting by id
// would make a hand-maintained, version-controlled governance.json (written
// once, with no ids, and re-applied against the same install repeatedly) fail
// its second apply with a 409 name conflict — the exact "config lives in git
// next to the rest of the install" workflow #1108 exists for. So ApplyGovernance
// reads the CURRENT state first, and: a doc profile whose Name matches an
// existing one is PUT to that existing row's real id (rename is therefore not
// expressible through apply — Name IS the identity a file's entries are
// matched against); a Name with no existing match is POSTed fresh. Either way,
// a profile whose Ceiling and Limits are BYTE-IDENTICAL to what is already
// stored is skipped entirely (governanceProfileUnchanged) — the no-op
// contract's other half, since the server audits every profile write
// unconditionally.
//
// An ASSIGNMENT carries no id on write (server-assigned, same as
// DriveGrantRequest), and is upserted purely by its own natural key
// (subject_type, subject) — skipped, the same way, when the existing row
// already names the same profile at the same priority. Its ProfileID is
// resolved against the SAME apply's own profile list before being sent: an id
// in doc.Assignments naming one of doc.Profiles's ORIGINAL (pre-write) ids is
// translated to that profile's real post-write id, so a document produced by
// GetGovernance against a POPULATED install reproduces both profiles and
// assignments when applied to an EMPTY one, even though the empty install
// mints entirely new profile ids. An assignment whose ProfileID names no
// profile in this same doc is sent exactly as given, trusting it as an
// already-real id on the target (the case for an assignments-only file, or one
// applied twice against the SAME install).
//
// prune, when true, additionally DELETES every server-side profile or
// assignment doc does not name (assignments first, since a profile still
// referenced by a to-be-pruned assignment fails the FK restrict). Without it —
// the default — nothing present server-side but absent from doc is touched,
// matching drive set's own "nothing the file omits is touched" rule.
//
// Every write's saved row replaces the caller's copy of doc in place, so a
// partial failure (returned as the second value) leaves doc's earlier entries
// holding what was actually persisted. On success, the returned document is a
// fresh GetGovernance — the authoritative post-write state.
func (c *Client) ApplyGovernance(ctx context.Context, doc GovernanceDocument, prune bool) (GovernanceDocument, error) {
	current, err := c.GetGovernance(ctx)
	if err != nil {
		return GovernanceDocument{}, fmt.Errorf("read current governance state: %w", err)
	}
	profileByName := make(map[string]GovernanceProfile, len(current.Profiles))
	for _, p := range current.Profiles {
		profileByName[p.Name] = p
	}
	assignmentByKey := make(map[string]GovernanceAssignment, len(current.Assignments))
	for _, a := range current.Assignments {
		assignmentByKey[governanceAssignmentKey(a.SubjectType, a.Subject)] = a
	}

	// fileIDToName/nameToRealID translate an assignment's ProfileID from
	// "whatever id this SAME file's profile entry carried" to "the id that
	// profile actually holds on THIS server" — see the doc comment above.
	fileIDToName := make(map[uuid.UUID]string, len(doc.Profiles))
	nameToRealID := make(map[string]uuid.UUID, len(doc.Profiles))

	for i, p := range doc.Profiles {
		saved := p
		if existing, ok := profileByName[p.Name]; ok && governanceProfileUnchanged(existing, p) {
			saved = existing
		} else {
			req := GovernanceProfileRequest{Name: p.Name, Ceiling: p.Ceiling, Limits: p.Limits, Contact: p.Contact}
			var resp GovernanceProfileResponse
			var werr error
			if ok {
				werr = c.do(ctx, http.MethodPut, "/api/v1/governance/profiles/"+existing.ID.String(), req, &resp)
			} else {
				werr = c.do(ctx, http.MethodPost, "/api/v1/governance/profiles", req, &resp)
			}
			if werr != nil {
				return GovernanceDocument{}, fmt.Errorf("apply governance profile %q: %w", p.Name, werr)
			}
			saved = resp.Profile
		}
		doc.Profiles[i] = saved
		if p.ID != uuid.Nil {
			fileIDToName[p.ID] = p.Name
		}
		nameToRealID[p.Name] = saved.ID
	}

	for i, a := range doc.Assignments {
		if name, ok := fileIDToName[a.ProfileID]; ok {
			if real, ok := nameToRealID[name]; ok {
				a.ProfileID = real
			}
		}
		key := governanceAssignmentKey(a.SubjectType, a.Subject)
		if existing, ok := assignmentByKey[key]; ok &&
			existing.ProfileID == a.ProfileID && existing.Priority == a.Priority {
			doc.Assignments[i] = existing
			continue
		}
		req := GovernanceAssignmentRequest{
			SubjectType: a.SubjectType, Subject: a.Subject,
			ProfileID: a.ProfileID, Priority: a.Priority,
		}
		var saved GovernanceAssignment
		if err := c.do(ctx, http.MethodPost, "/api/v1/governance/assignments", req, &saved); err != nil {
			return GovernanceDocument{}, fmt.Errorf("apply governance assignment (%s %q): %w", a.SubjectType, a.Subject, err)
		}
		doc.Assignments[i] = saved
	}

	if prune {
		keepAssignment := make(map[string]bool, len(doc.Assignments))
		for _, a := range doc.Assignments {
			keepAssignment[governanceAssignmentKey(a.SubjectType, a.Subject)] = true
		}
		// Assignments before profiles: a profile doc drops still fails the FK
		// restrict while a stale assignment of it survives.
		for _, a := range current.Assignments {
			if keepAssignment[governanceAssignmentKey(a.SubjectType, a.Subject)] {
				continue
			}
			if err := c.do(ctx, http.MethodDelete, "/api/v1/governance/assignments/"+a.ID.String(), nil, nil); err != nil {
				return GovernanceDocument{}, fmt.Errorf("prune governance assignment (%s %q): %w", a.SubjectType, a.Subject, err)
			}
		}
		keepProfile := make(map[string]bool, len(doc.Profiles))
		for _, p := range doc.Profiles {
			keepProfile[p.Name] = true
		}
		for _, p := range current.Profiles {
			if keepProfile[p.Name] {
				continue
			}
			if err := c.do(ctx, http.MethodDelete, "/api/v1/governance/profiles/"+p.ID.String(), nil, nil); err != nil {
				return GovernanceDocument{}, fmt.Errorf("prune governance profile %q: %w", p.Name, err)
			}
		}
	}

	return c.GetGovernance(ctx)
}
