// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 This file CONTAINS the compatibility fold: it reads the
// deprecated single-source scalars (Kind/Source/Ref/DefaultTarget/Writable, and
// the container kind) precisely so a pre-composition CLI or SDK caller keeps
// working. Deprecating them is what tells NEW callers to use Sources; the fold
// is the reason they can still be deprecated rather than deleted.

package api

import (
	"regexp"

	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// workspaceRequest is the POST/PUT body for a workspace. It is an ALIAS of the
// public SDK type, not a copy: the two cannot drift, and a field the server
// honors is reachable from the SDK by construction (a copy had already dropped
// llm_cred, which strict decoding then made unsettable from the SDK).
type workspaceRequest = client.WorkspaceRequest

// validateWorkspaceLLMCred checks an operator-supplied cred binding: a NAME
// only (whether the named Integration actually exists/resolves is W5's job —
// see resolveWorkspaceIntegration in llmcred.go). nil, or an empty
// IntegrationRef (clears the binding), is always valid.
func validateWorkspaceLLMCred(c *types.WorkspaceLLMCred) string {
	if c == nil || c.IntegrationRef == "" {
		return ""
	}
	if !repoFieldSafe(c.IntegrationRef) {
		return "llm_cred.integration_ref must not contain control characters or whitespace"
	}
	return ""
}

// defaultEphemeralTarget is the composition floor's in-sandbox scratch path
// when a workspace request declares no source at all.
const defaultEphemeralTarget = "/home/agent/work"

// maxBaseImageSteps / maxBaseImageStepLen cap a custom base image's layered
// Dockerfile lines — sane ceilings against a hostile/misbehaving request, not
// a sizing of any real recipe.
const (
	maxBaseImageSteps   = 32
	maxBaseImageStepLen = 2000
)

// decodeWorkspaceRequest decodes and validates a workspace request body.
// Unknown JSON fields are rejected (decodeStrictMsg, mirroring
// decodePolicyRequest's typo-safety) and everything is validated before any
// store write — workspaces are admin-gated onboarding config, so a bad source
// must never be persisted (fail closed).
//
// The legacy scalar shape (kind+source+ref+default_target+writable) is
// accepted and FOLDED into exactly one Sources entry (legacyWorkspaceSource) —
// this keeps `wardyn workspace create --kind local_dir --source /x` and old
// SDK callers working. Setting BOTH sources and any legacy field is a 400
// (mutually exclusive, so a caller can never have the two silently disagree).
// A request with NEITHER gets the composition floor: one ephemeral scratch
// source, never an error (a workspace always has at least one source).
// Each source is then validated by type (validateWorkspaceSource), and
// base_image is shape-guarded (validateWorkspaceBaseImage).
func decodeWorkspaceRequest(w http.ResponseWriter, r *http.Request) (workspaceRequest, string) {
	var req workspaceRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		return workspaceRequest{}, msg
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return workspaceRequest{}, "name is required"
	}

	legacy := req.Kind != "" || req.Source != "" || req.Ref != "" || req.DefaultTarget != "" || req.Writable
	switch {
	case legacy && len(req.Sources) > 0:
		return workspaceRequest{}, "sources and the legacy kind/source/ref/default_target/writable fields are mutually exclusive"
	case legacy:
		src, baseImage, msg := legacyWorkspaceSource(req)
		if msg != "" {
			return workspaceRequest{}, msg
		}
		req.Sources = []types.WorkspaceSource{src}
		if baseImage != nil {
			req.BaseImage = baseImage
		}
	case len(req.Sources) == 0:
		req.Sources = []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: defaultEphemeralTarget}}
	}

	seenTargets := make(map[string]int, len(req.Sources))
	for i, src := range req.Sources {
		if msg := validateWorkspaceSource(src); msg != "" {
			return workspaceRequest{}, fmt.Sprintf("sources[%d]: %s", i, msg)
		}
		// W8-S1-3 (mirrors validatePolicyWorkspaces' own unique-target
		// invariant, policy.go): an EXPLICIT target shared by two sources
		// resolves to the same in-sandbox mount/clone path, which
		// validatePolicyWorkspaces then 422s on every subsequent run — an
		// onboarding-time 400 naming both sources catches it before it's
		// even possible to run. An EMPTY target (no explicit choice — the
		// caller relies on the per-attach default) is deliberately not
		// checked here, same as validatePolicyWorkspaces: the default dest
		// isn't derived until attach/clone time (buildRepoRecords).
		if src.Target == "" {
			continue
		}
		if j, dup := seenTargets[src.Target]; dup {
			return workspaceRequest{}, fmt.Sprintf("sources[%d]: target %q duplicates sources[%d]", i, src.Target, j)
		}
		seenTargets[src.Target] = i
	}
	if msg := validateWorkspaceBaseImage(req.BaseImage); msg != "" {
		return workspaceRequest{}, msg
	}
	if msg := validateWorkspaceLLMCred(req.LLMCred); msg != "" {
		return workspaceRequest{}, msg
	}
	return req, ""
}

// legacyWorkspaceSource folds the pre-composition-model scalar shape
// (kind+source+ref+default_target+writable) into exactly one WorkspaceSource.
// A legacy "container" kind — a bring-your-own base image with no mount — was
// the pre-composition shape for what is now an ephemeral source plus a "byo"
// BaseImage (WorkspaceSourceType has no "container" of its own).
func legacyWorkspaceSource(req workspaceRequest) (types.WorkspaceSource, *types.WorkspaceBaseImage, string) {
	source := strings.TrimSpace(req.Source)
	if source == "" {
		return types.WorkspaceSource{}, nil, "source is required"
	}
	switch req.Kind {
	case types.WorkspaceKindLocalDir:
		return types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeLocalDir, Path: source,
			Target: req.DefaultTarget, Writable: req.Writable,
		}, nil, ""
	case types.WorkspaceKindRepo:
		return types.WorkspaceSource{
			Type: types.WorkspaceSourceTypeRepo, Source: source,
			Ref: req.Ref, Target: req.DefaultTarget,
		}, nil, ""
	case types.WorkspaceKindContainer:
		return types.WorkspaceSource{Type: types.WorkspaceSourceTypeEphemeral, Target: req.DefaultTarget},
			&types.WorkspaceBaseImage{Kind: "byo", Image: source}, ""
	default:
		return types.WorkspaceSource{}, nil, `kind must be "local_dir", "repo", or "container"`
	}
}

// validateWorkspaceSource runs the same safety checks the run-creation path
// already enforces on the equivalent free-text field, per source Type:
// local_dir reuses runner.ValidateMountSource on Path (the host bind-mount
// deny-list — onboarding vets the reusable host PATH; the per-run target is
// chosen later, per-attach); repo reuses repoFieldSafe + repoCloneURL
// (runs.go), the same pair that gates AgentRun.Repo today; ephemeral has no
// host/repo value to check. An optional Target is validated via
// runner.ValidateTarget for every type, since it becomes an in-container
// mount/clone/scratch-dir path once a run attaches this workspace.
func validateWorkspaceSource(src types.WorkspaceSource) string {
	switch src.Type {
	case types.WorkspaceSourceTypeLocalDir:
		if strings.TrimSpace(src.Path) == "" {
			return "path is required for a local_dir source"
		}
		if err := runner.ValidateMountSource(src.Path); err != nil {
			return "invalid path: " + err.Error()
		}
	case types.WorkspaceSourceTypeRepo:
		if strings.TrimSpace(src.Source) == "" {
			return "source is required for a repo source"
		}
		if !repoFieldSafe(src.Source) {
			return "source must not contain control characters or whitespace"
		}
		if repoCloneURL(src.Source) == "" {
			return "source is not a recognized repo slug or http(s) clone URL"
		}
	case types.WorkspaceSourceTypeEphemeral:
		// no host/repo value to validate — target only, below.
	default:
		return `type must be "local_dir", "repo", or "ephemeral"`
	}
	if src.Target != "" {
		if err := runner.ValidateTarget(src.Target); err != nil {
			return "invalid target: " + err.Error()
		}
	}
	// WSPIPE-7: Overrides' three values are a closed set (workspace_contract.go);
	// a key the source doesn't (yet) declare is a harmless no-op by construction
	// (FoldWorkspaceContract only ever consults Overrides against ITS source's
	// own requirement keys), so only the VALUE is worth rejecting — a garbage
	// value there would otherwise silently no-op forever with a 200 and no signal.
	for key, stance := range src.Overrides {
		if stance != types.OverrideOff && stance != types.OverrideOptional && stance != types.OverrideRequired {
			return fmt.Sprintf("overrides[%q]: must be %q, %q, or %q", key, types.OverrideOff, types.OverrideOptional, types.OverrideRequired)
		}
	}
	return ""
}

// validateWorkspaceBaseImage shape-guards an onboarded workspace's base-image
// choice. A non-"recommended" Image is checked with the same charset guard the
// legacy "container" kind used for its image ref (repoFieldSafe: no control
// characters/whitespace) — the daemon validates the ref for real at
// pull/build time. Steps are only meaningful (and only capped) for "custom".
func validateWorkspaceBaseImage(b *types.WorkspaceBaseImage) string {
	if b == nil {
		return ""
	}
	switch b.Kind {
	case "recommended", "registry", "custom", "byo":
	default:
		return `base_image.kind must be "recommended", "registry", "custom", or "byo"`
	}
	if b.Kind != "recommended" {
		if strings.TrimSpace(b.Image) == "" {
			return "base_image.image is required for kind " + b.Kind
		}
		if !repoFieldSafe(b.Image) {
			return "base_image.image must not contain control characters or whitespace"
		}
	}
	if len(b.Steps) > 0 {
		if b.Kind != "custom" {
			return "base_image.steps is only valid for kind=custom"
		}
		if len(b.Steps) > maxBaseImageSteps {
			return fmt.Sprintf("base_image.steps: too many steps (max %d)", maxBaseImageSteps)
		}
		for _, step := range b.Steps {
			if len(step) > maxBaseImageStepLen {
				return fmt.Sprintf("base_image.steps: a step exceeds the max length (%d)", maxBaseImageStepLen)
			}
		}
	}
	return ""
}

// handleListWorkspaces returns onboarded workspaces in reverse creation order,
// paginated by ?limit=&offset= (see parseListPage).
func (s *Server) handleListWorkspaces(w http.ResponseWriter, r *http.Request) {
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	var pageFn func(store.Page) ([]types.Workspace, error)
	if pg, ok := s.cfg.Store.(store.Pager); ok {
		pageFn = func(p store.Page) ([]types.Workspace, error) { return pg.ListWorkspacesPage(r.Context(), p) }
	}
	servePage(w, page, pageFn, func() ([]types.Workspace, error) { return s.cfg.Store.ListWorkspaces(r.Context()) })
}

// handleGetWorkspace returns one workspace by id (404 when unknown).
func (s *Server) handleGetWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	// Repair-on-read: settle any import run stranded by a terminal-transition path
	// with no reconcile hook — a record entry stuck `recording`, or a workspace
	// stuck `verifying`/`scanning` whose run already terminated (the idle reaper, a
	// crashed watcher, or a dispatch-time failure that raced a wardynd crash). The
	// reconcilers are idempotent + CAS-guarded, and this only fires on a real strand.
	ws = s.repairStaleWorkspaceRuns(r.Context(), ws)
	// The import panel renders Record Mode from the workspace's own record_results
	// map (per-session state); sessions are user-named, not a derived taxonomy.
	writeJSON(w, http.StatusOK, ws)
}

// sshWorkspaceSourcesReady returns a 400-worthy message when any repo source's
// clone URL is an SSH URL to a supported provider but the operator has not yet
// stored the canonical ssh-key-<host> secret the clone needs — rejecting at
// onboarding instead of accepting a workspace whose every scan/verify/record clone
// would then fail. "" = fine (no SSH repo sources, or every needed secret is
// present). It runs AFTER decodeWorkspaceRequest (which already rejects an SSH
// URL to an unsupported host via repoCloneURL). Shared by create + update so
// switching an existing workspace's source to SSH is covered too.
func (s *Server) sshWorkspaceSourcesReady(ctx context.Context, sources []types.WorkspaceSource) string {
	var names []string
	namesLoaded := false
	for _, src := range sources {
		if src.Type != types.WorkspaceSourceTypeRepo {
			continue
		}
		host, ok := sshCloneHost(src.Source)
		if !ok {
			continue
		}
		secretName, ok := canonicalSSHKeySecret(host)
		if !ok {
			continue
		}
		if !namesLoaded {
			var err error
			names, err = s.listUserSecretNames(ctx)
			if err != nil {
				return "" // don't hard-block onboarding on a transient secret-store read error; the run-time grant path still gates the actual clone
			}
			namesLoaded = true
		}
		if slices.Contains(names, secretName) {
			continue
		}
		return "SSH source needs the " + secretName + " secret first — store your private key via setup's SCM import or `wardyn secret set " + secretName + "`"
	}
	return ""
}

// handleCreateWorkspace validates the request and onboards a new workspace in
// pending_scan status. Returns 201 with the created row, or 400 on an invalid
// body/source. The real scan (populating Profile, flipping status to
// scanned/scanning/error) happens via the separate POST /workspaces/{id}/scan
// endpoint (see handleScanWorkspace) — creation never scans inline.
func (s *Server) handleCreateWorkspace(w http.ResponseWriter, r *http.Request) {
	req, msg := decodeWorkspaceRequest(w, r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if msg := s.sshWorkspaceSourcesReady(r.Context(), req.Sources); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	now := s.cfg.Now().UTC()
	id := uuid.New()
	ws := types.Workspace{
		ID:        id,
		Name:      req.Name,
		Sources:   req.Sources,
		BaseImage: normalizeRecommended(req.BaseImage),
		LLMCred:   req.LLMCred,
		Status:    types.WorkspacePendingScan,
		CreatedAt: now,
		UpdatedAt: now,
	}
	// Three-tier split: the embedded write becomes "library upsert + attach" —
	// the same dir/repo named by two workspaces is ONE library entry with ONE
	// contract. The embedded columns are still written too (expand posture;
	// 0032 drops them), and the store's hydrate pass makes attachments the
	// authoritative read the moment they exist. No prior attachments to carry
	// Overrides forward from — this is a brand-new workspace.
	atts, baseImageID, aerr := s.upsertAndAttach(r, req.Sources, req.BaseImage, nil)
	if aerr != nil {
		writeError(w, http.StatusInternalServerError, "attach sources: "+aerr.Error())
		return
	}
	ws.Attachments, ws.BaseImageID = atts, baseImageID
	created, err := s.cfg.Store.CreateWorkspace(r.Context(), ws)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create workspace: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.create", id.String(), "success", mustJSON(map[string]any{
			"name": created.Name, "sources": len(created.Sources),
		})))
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateWorkspace replaces a workspace's editable identity fields (name,
// sources, base_image), round-tripping the fetched row so the scan-owned
// fields survive (the store UPDATE replaces every column — the old
// construct-from-scratch call zeroed status, violating its CHECK constraint).
// When the COMPOSITION (sources) or the BASE IMAGE changes, the scan state
// (profile/image/status), the requirements contract, and the operator's
// egress approvals are reset: all were reviewed against the OLD content and
// must be re-earned. LLMCred is CREATE-ONLY (see workspaceRequest.LLMCred) and
// is deliberately left untouched here. Returns 404 when unknown, 400 on an
// invalid body/source.
func (s *Server) handleUpdateWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	req, msg := decodeWorkspaceRequest(w, r)
	if msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	if msg := s.sshWorkspaceSourcesReady(r.Context(), req.Sources); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	// this GET→mutate→UPDATE can race an async repo-scan upload and
	// write back a stale profile; identity edits are rare and the remedy is a
	// re-scan — add an optimistic updated_at guard if it ever bites for real.
	sourcesChanged := !slices.EqualFunc(ws.Sources, req.Sources, workspaceSourceContentEqual)
	imageChanged := !baseImageEqual(ws.BaseImage, req.BaseImage)
	ws.Name, ws.Sources, ws.BaseImage = req.Name, req.Sources, normalizeRecommended(req.BaseImage)
	// Three-tier: the edited composition upserts+attaches through the library
	// exactly as create does — the hydrated read makes attachments
	// authoritative, so they must track every composition edit. existing
	// (the PRE-edit attachments) carries any per-source Overrides forward by
	// SourceID (WSPIPE-7) — must be read before the next line overwrites
	// ws.Attachments.
	atts, baseImageID, aerr := s.upsertAndAttach(r, req.Sources, req.BaseImage, ws.Attachments)
	if aerr != nil {
		writeError(w, http.StatusInternalServerError, "attach sources: "+aerr.Error())
		return
	}
	ws.Attachments, ws.BaseImageID = atts, baseImageID
	if sourcesChanged {
		// New CONTENT: everything reviewed against the old sources is stale.
		// (Tier-1 contracts are untouched — they live on the sources.)
		//
		// Profile and Status are NOT reset here (STORE-4): every workspace
		// reaching this handler now has a non-empty Attachments (upsertAndAttach
		// always attaches at least the composition floor's ephemeral source), so
		// hydrate's attachment-backed branch unconditionally RE-DERIVES both from
		// the fresh sources on every read — a write here is provably discarded
		// before this request's own response leaves the store.
		ws.ApprovedEgress = nil
		ws.Requirements = nil
		ws.RecordResults = nil
	}
	if sourcesChanged || imageChanged {
		// The build cache keys on the old profile/base — a different base
		// image alone invalidates it, but does NOT wipe the contract: in the
		// three-tier model requirements come from the sources + integrations,
		// and the wizard persists its step-② image choice through here — a
		// first-time pick must not destroy the overlay it just helped shape.
		//
		// bug-workspace-1: the docker tag this row was pointing at is about
		// to become unreachable from the store (no run will ever resolve
		// this ref again — resolveWorkspaceImage rebuilds fresh next launch)
		// — reclaim it now rather than leaking it forever.
		s.removeStaleImage(r.Context(), ws.ImageRef, "")
		ws.ImageRef = ""
		ws.BuiltProfileHash = ""
	}
	updated, err := s.cfg.Store.UpdateWorkspace(r.Context(), id, ws)
	if notFoundIf(w, err, "workspace") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "update workspace: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.update", id.String(), "success", mustJSON(map[string]any{
			"name": updated.Name, "sources": len(updated.Sources), "rescan_required": sourcesChanged, "image_changed": imageChanged,
		})))
	writeJSON(w, http.StatusOK, updated)
}

// workspaceSourceContentEqual compares everything about a WorkspaceSource
// EXCEPT Overrides: content — Type/Path/Source/Ref/Target/Writable — is what
// "sourcesChanged" (handleUpdateWorkspace) means by "everything reviewed
// against the old sources is stale". A plain Overrides edit changes which
// requirement rows apply, never what's mounted, so it must not itself reset
// ApprovedEgress/Requirements/RecordResults — and types.WorkspaceSource's
// Overrides map makes the type non-comparable, so slices.Equal (which needs
// `comparable`) can no longer compare a []WorkspaceSource directly.
func workspaceSourceContentEqual(a, b types.WorkspaceSource) bool {
	return a.Type == b.Type && a.Path == b.Path && a.Source == b.Source &&
		a.Ref == b.Ref && a.Target == b.Target && a.Writable == b.Writable
}

// normalizeRecommended collapses an explicit {"kind":"recommended"} — what
// the wizard's continueFromSources/continueFromImage steps always send
// (wizard-types.ts's baseImageRequest) — onto nil, the shape hydrateWorkspace
// reads back (store_sources.go: ws.BaseImage=nil unless BaseImageID is set;
// upsertAndAttach deliberately leaves BaseImageID nil for kind "recommended").
// Without this, write and hydrated-read disagreed on one spelling of
// "recommended" (WSPIPE-2): baseImageEqual saw the wizard's explicit form as
// different from the hydrated nil on every edit and threw away the built
// image, and the legacy embedded base_image column stored a non-NULL blob a
// pre-split row would never have carried.
func normalizeRecommended(b *types.WorkspaceBaseImage) *types.WorkspaceBaseImage {
	if b != nil && b.Kind == "recommended" {
		return nil
	}
	return b
}

// baseImageEqual reports whether two base-image choices are equivalent
// (nil-safe; Steps compared by content; "recommended" normalized to nil on
// both sides first, so the wizard's explicit form and the hydrated nil never
// read as a change — WSPIPE-2).
func baseImageEqual(a, b *types.WorkspaceBaseImage) bool {
	a, b = normalizeRecommended(a), normalizeRecommended(b)
	if a == nil || b == nil {
		return a == b
	}
	return a.Kind == b.Kind && a.Image == b.Image && slices.Equal(a.Steps, b.Steps)
}

// maxApprovedEgress bounds the operator-owned approved-egress list — matches
// the scanner's SuggestedEgress cap, its only intended feeder.
const maxApprovedEgress = 64

// handleSetApprovedEgress replaces the workspace's operator-owned approved
// egress list (PUT semantics: the body is the FULL list; un-approve by
// omission — idempotent, no per-host delete endpoint needed). These hosts
// originate from the scanner's content-derived SuggestedEgress — untrusted
// repo content — so promotion is an explicit operator action, audited, and
// the list lives OUTSIDE the scan-owned profile blob: a rescan can neither
// widen nor resurrect it. Plain lowercase dotted hosts only.
func (s *Server) handleSetApprovedEgress(w http.ResponseWriter, r *http.Request) {
	type body struct {
		Domains []string `json:"domains"`
	}
	// W19-W19b-3: a host the git broker (or the control plane itself) already
	// owns is DEAD BY CONSTRUCTION as a direct ApprovedEgress entry — dispatch
	// routes github.com/api.github.com/codeload.github.com/*.githubusercontent.com
	// and every SSH-over-443 forge host through the broker/proxy specially
	// (runs_dispatch_gitbroker.go), never as a plain allowlist host, so
	// "approving" one here writes a row a real run's proxy will never consult.
	// This is the SAME static skip-set promoteSkipHosts applies to the bulk
	// promote-egress writer (record.go) plus the control-plane's own host
	// (handlePromoteRecordEgress's selfHost) — this is the LAST writer of
	// ApprovedEgress that did not share it; reject with the same honest 4xx
	// the shape validator already uses instead of a silent-toast no-op.
	deadHosts := map[string]struct{}{}
	for _, h := range gitBrokerManagedHosts {
		deadHosts[strings.ToLower(h)] = struct{}{}
	}
	for _, h := range gitBrokerSSHHosts() {
		deadHosts[strings.ToLower(h)] = struct{}{}
	}
	if selfHost := controlPlaneHost(s.cfg.ControlPlaneURL); selfHost != "" {
		deadHosts[selfHost] = struct{}{}
	}
	scopedWorkspaceWrite(s, w, r, "workspace.egress.approve",
		func(req body) ([]string, string) {
			if len(req.Domains) > maxApprovedEgress {
				return nil, "too many domains (max 64)"
			}
			set := map[string]struct{}{}
			for _, d := range req.Domains {
				d = strings.ToLower(strings.TrimSpace(d))
				if !workspacescan.ValidApprovedHost(d) {
					return nil, "invalid domain (plain lowercase host, no scheme/port/wildcard): " + d
				}
				if _, dead := deadHosts[d]; dead {
					return nil, "host " + d + " is already routed specially (git broker / control plane) — a direct ApprovedEgress entry for it is never consulted"
				}
				set[d] = struct{}{}
			}
			return sortedKeys(set), ""
		},
		// Wrapped, not passed as a method value: the store call must not be
		// resolved until validation has passed.
		func(ctx context.Context, id uuid.UUID, doms []string) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceApprovedEgress(ctx, id, doms)
		},
		func(domains []string) map[string]any { return map[string]any{"domains": domains} })
}

// handleSetWorkspaceLLMCred binds (or clears) the operator-owned model/harness
// credential for a workspace/container — the scoped write behind
// PUT /workspaces/{id}/llm-cred. A run that picks this workspace inherits the
// binding (applyWorkspaceCreds). Body: a WorkspaceLLMCred; mode="" (or null)
// clears it. Names/refs only — the secret itself lives in the store.
func (s *Server) handleSetWorkspaceLLMCred(w http.ResponseWriter, r *http.Request) {
	scopedWorkspaceWrite(s, w, r, "workspace.llm_cred.set",
		func(req types.WorkspaceLLMCred) (*types.WorkspaceLLMCred, string) {
			if msg := validateWorkspaceLLMCred(&req); msg != "" {
				return nil, msg
			}
			if req.IntegrationRef == "" {
				return nil, "" // nil clears the binding
			}
			return &req, ""
		},
		func(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceLLMCred(ctx, id, cred)
		},
		func(cred *types.WorkspaceLLMCred) map[string]any {
			ref := ""
			if cred != nil {
				ref = cred.IntegrationRef
			}
			return map[string]any{"integration_ref": ref}
		})
}

// integrationRefRE is the contract-side integration reference grammar:
// operator-authored slugs (secretNameRE's charset) plus up to three
// colon-joined qualifier segments — the shapes Wardyn's own legacy adoption
// mints and stores verbatim. 128-char cap matches the id rule.
var integrationRefRE = regexp.MustCompile(`^[a-z0-9._-]{1,128}(:[a-z0-9._-]{1,64}){0,3}$`)

// maxWorkspaceRequirements bounds the requirements-contract map a single PUT
// may set — a sane ceiling against a hostile/misbehaving request, not a sizing
// of any real contract.
const maxWorkspaceRequirements = 256

// validRequirementLevel / validRequirementProvenance are WorkspaceRequirement's
// two closed enums (types.go documents both on WorkspaceRequirement).
func validRequirementLevel(l string) bool      { return l == "required" || l == "optional" }
func validRequirementProvenance(p string) bool { return p == "scan_seeded" || p == "operator_set" }

// splitRequirementKey splits a Workspace.Requirements key on the FIRST colon
// into its fixed type prefix ("secret"|"egress"|"write") and suffix, per the
// grammar documented on types.Workspace.Requirements — never the LAST colon: a
// write:<path> suffix may itself legally contain colons. ok=false when there
// is no colon, the suffix is empty, or the prefix isn't one of the three known
// tokens. Shared with the fold this contract feeds (runs_create.go's
// applyWorkspaceRequirements) and the preflight checklist escalation
// (compose_setup.go's setupWorkspaceSecretItems).
func splitRequirementKey(key string) (typ, rest string, ok bool) {
	// The grammar moved home to internal/types (the store's hydrate pass folds
	// contracts and cannot import api); this alias keeps api's call sites put.
	return types.SplitRequirementKey(key)
}

// validateWorkspaceRequirement validates one Workspace.Requirements key+value
// pair before it is ever persisted: the key's grammar (splitRequirementKey),
// the suffix shape appropriate to its type — egress host via
// workspacescan.ValidApprovedHost (the SAME rule approved-egress promotion
// enforces: a plain lowercase host, no scheme/port/wildcard — deliberately
// stricter than a run policy's AllowedDomains, which also accepts a
// "*."-wildcard; a requirement is written here through an operator-gated
// endpoint like approved-egress, so the same conservative rule applies),
// secret name via the existing secret-name rule (validSecretRef: secretNameRE
// plus the reserved-platform-name guard), write path via
// runner.ValidateMountSource (the same host bind-mount deny-list a policy
// mount is checked against) — and the value's level/provenance enums. A
// non-empty return is the 400 message.
func validateWorkspaceRequirement(key string, req types.WorkspaceRequirement) string {
	typ, rest, ok := splitRequirementKey(key)
	if !ok {
		return fmt.Sprintf("invalid requirement key %q (want secret:<name>, egress:<host>, write:<path>, or integration:<id>)", key)
	}
	switch typ {
	case "secret":
		// sinkReservedSecret, not validSecretRef (WSPIPE-6): this key feeds
		// applyRequiredSecretGrant, a real credential SINK (runs_create.go),
		// which every other sink (policy.go, inline_policy.go) guards with
		// sinkReservedSecret — validSecretRef's plain reservedSecret misses the
		// three resident AWS SigV4 names, so a "required" row naming one
		// validated clean here and only 403'd at the run's first model call,
		// the real reason buried in an audit event instead of this write-time 400.
		if !secretNameRE.MatchString(rest) || sinkReservedSecret(rest) {
			return fmt.Sprintf("requirement %q: invalid secret name", key)
		}
	case "integration":
		// Shape only. EXISTENCE is deliberately not checked here: a workspace
		// may name an integration before it is configured (the contract states
		// an intent), and the fold degrades silently to "opens nothing" until
		// the row exists. Requiring it to exist first would make ordering the
		// operator's problem.
		//
		// The ref grammar is WIDER than an operator-authored id
		// (validateIntegrationWrite's secretNameRE): Wardyn itself mints
		// colon-qualified ids for ADOPTED legacy rows
		// ("anthropic_subscription:managed", "git_host:<host>") and stores
		// them verbatim — a contract must
		// be able to name what the store holds. Split on the FIRST colon at
		// the key layer keeps this unambiguous.
		if !integrationRefRE.MatchString(rest) {
			return fmt.Sprintf("requirement %q: invalid integration id", key)
		}
	case "egress":
		if !workspacescan.ValidApprovedHost(rest) {
			return fmt.Sprintf("requirement %q: invalid egress host (plain lowercase host, no scheme/port/wildcard)", key)
		}
	case "write":
		if err := runner.ValidateMountSource(rest); err != nil {
			return fmt.Sprintf("requirement %q: invalid write path: %s", key, err.Error())
		}
	}
	if !validRequirementLevel(req.Level) {
		return fmt.Sprintf("requirement %q: level must be \"required\" or \"optional\"", key)
	}
	if !validRequirementProvenance(req.Provenance) {
		return fmt.Sprintf("requirement %q: provenance must be \"scan_seeded\" or \"operator_set\"", key)
	}
	return ""
}

// handleSetWorkspaceRequirements replaces the workspace's requirements
// contract — the scoped write behind PUT /workspaces/{id}/requirements. Body:
// {"requirements": {"<type>:<key>": {"level":"required"|"optional",
// "provenance":"scan_seeded"|"operator_set"}, ...}}. Every key+value pair is
// validated (validateWorkspaceRequirement) before the store write; the map
// itself is capped (maxWorkspaceRequirements). See types.Workspace.Requirements
// for the key grammar and runs_create.go's applyWorkspaceRequirements for the
// load-bearing fold this contract feeds into a run's resolved policy.
func (s *Server) handleSetWorkspaceRequirements(w http.ResponseWriter, r *http.Request) {
	type body struct {
		Requirements map[string]types.WorkspaceRequirement `json:"requirements"`
	}
	scopedWorkspaceWrite(s, w, r, "workspace.requirements.write",
		func(req body) (map[string]types.WorkspaceRequirement, string) {
			if len(req.Requirements) > maxWorkspaceRequirements {
				return nil, fmt.Sprintf("too many requirements (max %d)", maxWorkspaceRequirements)
			}
			for _, key := range sortedKeys(req.Requirements) {
				if msg := validateWorkspaceRequirement(key, req.Requirements[key]); msg != "" {
					return nil, msg
				}
			}
			return req.Requirements, ""
		},
		// Wrapped, not passed as a method value: the store call must not be
		// resolved until validation has passed.
		func(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceRequirements(ctx, id, s.dropSourceContributedScanSeeded(ctx, id, reqs))
		},
		func(reqs map[string]types.WorkspaceRequirement) map[string]any {
			return map[string]any{"count": len(reqs)}
		})
}

// dropSourceContributedScanSeeded strips provenance:"scan_seeded" rows from
// reqs whose key an ATTACHED SOURCE already contributes to this workspace's
// fold — WSPIPE-3's server-side belt. The overlay this endpoint writes is
// meant to carry only the OPERATOR's own edits (setRequirementLane always
// stamps operator_set); a scan_seeded row here can only be the wizard's
// client-side seeding, which FoldWorkspaceContract's rule 6 makes win over
// the SOURCE's own (correctly rescanned) contract forever — a name a rescan
// drops from the source stays stuck in the overlay with no way to remove it.
// Dropped silently, never rejected: the wizard still sends these until its
// own fix lands (source_scan.go's design note), and a dropped row is
// provably redundant — the source already contributes it — never a lost
// operator intent, unlike WSPIPE-8's identity-hit case. Best-effort: a store
// read error leaves reqs untouched (fail OPEN on the belt; the write itself
// must not become unavailable because of it).
func (s *Server) dropSourceContributedScanSeeded(ctx context.Context, id uuid.UUID, reqs map[string]types.WorkspaceRequirement) map[string]types.WorkspaceRequirement {
	hasScanSeeded := false
	for _, req := range reqs {
		if req.Provenance == "scan_seeded" {
			hasScanSeeded = true
			break
		}
	}
	if !hasScanSeeded {
		return reqs
	}
	ws, err := s.cfg.Store.GetWorkspace(ctx, id)
	if err != nil || len(ws.Attachments) == 0 {
		return reqs
	}
	var sourceIDs []uuid.UUID
	for _, att := range ws.Attachments {
		if att.SourceID != nil {
			sourceIDs = append(sourceIDs, *att.SourceID)
		}
	}
	sources, err := s.cfg.Store.GetSourcesByIDs(ctx, sourceIDs)
	if err != nil {
		return reqs
	}
	contributed := map[string]bool{}
	for _, src := range sources {
		for key := range src.Requirements {
			contributed[key] = true
		}
	}
	out := make(map[string]types.WorkspaceRequirement, len(reqs))
	for key, req := range reqs {
		if req.Provenance == "scan_seeded" && contributed[key] {
			continue // redundant: an attached source's OWN contract already carries this
		}
		out[key] = req
	}
	return out
}

// Observed-egress synthesis bounds: scan the most recent runs that reference
// this workspace and, for each, at most this many audit events.
const (
	maxObservedRuns        = 50
	maxObservedAuditPerRun = 500
)

// handleObservedEgress synthesizes least-privilege egress feedback from run
// TELEMETRY (the pattern: run permissive, then tighten/expand from observed
// evidence): it returns the egress hosts that runs using THIS workspace were
// actually DENIED, minus what the workspace already allows or the operator
// already approved. These are candidates an operator can promote into the
// workspace's approved-egress list. Read-only and advisory — it never widens
// anything itself.
func (s *Server) handleObservedEgress(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}

	// Already-satisfied hosts to subtract: the scanned profile's auto-allowed
	// egress plus the operator's approvals.
	allowed := map[string]bool{}
	for _, d := range ws.ApprovedEgress {
		allowed[d] = true
	}
	if p, ok := workspaceProfile(ws); ok {
		for _, d := range p.EgressDomains {
			allowed[d] = true
		}
	}

	runs, err := s.cfg.Store.ListRuns(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list runs: "+err.Error())
		return
	}
	denied := map[string]struct{}{}
	scanned := 0
	for _, run := range runs {
		if scanned >= maxObservedRuns {
			break
		}
		if !runUsesWorkspace(run, ws) {
			continue
		}
		scanned++
		events, aerr := s.cfg.Store.QueryAuditEvents(r.Context(), run.ID, maxObservedAuditPerRun)
		if aerr != nil {
			continue // best-effort per run
		}
		for _, ev := range events {
			if ev.Action != "egress.deny" {
				continue
			}
			host := strings.ToLower(strings.TrimSpace(ev.Target))
			if host == "" || allowed[host] || !workspacescan.ValidApprovedHost(host) {
				continue
			}
			denied[host] = struct{}{}
		}
	}
	out := sortedKeys(denied)
	writeJSON(w, http.StatusOK, map[string]any{"denied": out, "runs_examined": scanned})
}

// runUsesWorkspace reports whether a run referenced ws, using the denormalized
// run fields (WorkspacePath = the primary local-dir source; Repo = the repo
// slug/URL) against EVERY one of ws's sources — not just the single-source
// Kind/Source mirror, which is empty for a multi-source workspace. Only the
// PRIMARY workspace is linked on the run, so observed telemetry is scoped to
// runs where ws was primary — a deliberate, honest limit (secondary
// mounts/repos aren't denormalized onto the run).
func runUsesWorkspace(run types.AgentRun, ws types.Workspace) bool {
	for _, src := range ws.Sources {
		switch src.Type {
		case types.WorkspaceSourceTypeLocalDir:
			if run.WorkspacePath != "" && run.WorkspacePath == src.Path {
				return true
			}
		case types.WorkspaceSourceTypeRepo:
			if run.Repo != "" && run.Repo == src.Source {
				return true
			}
		}
	}
	return false
}

// handleWriteEnvAsCode generates committable env-as-code from the workspace's
// CURRENT scanned profile (base + language features + artifact-registry
// redirects, plus an AGENTS.md documenting the detected toolchain/commands) and
// writes it into the host source dir — the host-write half of what used to be
// handleFinalizeWorkspace's optional emit, extracted on its own now that there
// is no more "finalize to ready" step. LOCAL-DIR ONLY: a repo-only workspace
// has no host path to write into (regenerate + commit yourself via
// GET /workspaces/{id}/env-as-code, which stays the read path regardless of
// composition). Writes to the FIRST local_dir source when the workspace has
// more than one.
func (s *Server) handleWriteEnvAsCode(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	localDirs := workspaceSourcesOfType(ws, types.WorkspaceSourceTypeLocalDir)
	if len(localDirs) == 0 {
		writeError(w, http.StatusUnprocessableEntity,
			"env-as-code can only be written to disk for a workspace with a local_dir source (a repo/ephemeral-only "+
				"workspace has no host path — use GET /workspaces/{id}/env-as-code and commit the files yourself)")
		return
	}
	files, ok := s.envAsCodeFor(w, r, ws)
	if !ok {
		return
	}
	skipped, werr := writeEnvAsCode(localDirs[0].Path, files)
	if werr != nil {
		writeError(w, http.StatusInternalServerError, "write env-as-code: "+werr.Error())
		return
	}
	// written_files must name only what was actually written — a skipped key
	// (an operator file writeEnvAsCode refused to clobber) staying in this map
	// would tell the caller it was overwritten when it was not.
	for _, rel := range skipped {
		delete(files, rel)
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.envcode.write", id.String(), "success",
		mustJSON(map[string]any{"files": len(files), "skipped": len(skipped)})))
	writeJSON(w, http.StatusOK, map[string]any{"written_files": files, "skipped_files": skipped})
}

// envAsCodeFor generates the committable env-as-code for a workspace from its
// CURRENT scanned profile. Shared by handleWriteEnvAsCode and
// handleGetEnvAsCode so the two generations can never drift. It writes its own
// error response (422 when the workspace has no scanned profile, 500 when the
// generator fails) and returns ok=false, mirroring getWorkspaceOr404.
func (s *Server) envAsCodeFor(w http.ResponseWriter, r *http.Request, ws types.Workspace) (map[string]string, bool) {
	profile, ok := workspaceProfile(ws)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "workspace has no scanned profile to emit from")
		return nil, false
	}
	// Fold the operator-wide artifact-registry redirects (URL-only) into the
	// committable output so an exported workspace pulls from the corp mirror.
	// Best-effort: a store error / no site-config just omits them.
	var artifactBases map[string]string
	if s.cfg.Store != nil {
		if sc, scErr := s.cfg.Store.GetSiteConfig(r.Context()); scErr == nil {
			artifactBases = artifactBaseURLs(sc)
		}
	}
	// baseRef: the SAME "explicit non-recommended choice" predicate
	// resolveWorkspaceImage uses (workspace_run.go) — WITHOUT it every export
	// described the generic devcontainer base regardless of what this
	// workspace's own registry/custom/byo pick actually boots (WSPIPE-9).
	var baseRef string
	if b := ws.BaseImage; b != nil && b.Kind != "recommended" && strings.TrimSpace(b.Image) != "" {
		baseRef = b.Image
	}
	files, gerr := workspacescan.EmitEnvAsCode(profile, artifactBases, baseRef)
	if gerr != nil {
		writeError(w, http.StatusInternalServerError, "generate env-as-code: "+gerr.Error())
		return nil, false
	}
	return files, true
}

// handleGetEnvAsCode re-generates the committable env-as-code for a workspace.
// Finalize hands these files back exactly once, in its response body, and a repo
// workspace has nowhere on the host to write them — so without this the content
// the operator is meant to COMMIT dies with the import dialog. Nothing is
// persisted: the files are deterministic from stored state, so this reflects a
// later re-scan or setup-command edit rather than a finalize-time snapshot.
func (s *Server) handleGetEnvAsCode(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	files, ok := s.envAsCodeFor(w, r, ws)
	if !ok {
		return
	}
	// Same key finalize returns, so a client renders either response identically.
	writeJSON(w, http.StatusOK, map[string]any{"emitted_files": files})
}

// writeEnvAsCode writes generated env-as-code files under root, returning the
// subset of keys it left untouched because they already existed. Paths are
// the fixed, safe outputs of EmitEnvAsCode (.devcontainer/devcontainer.json,
// AGENTS.md, plus any artifact-redirect config like .npmrc/.cargo/config.toml).
//
// Every write goes through os.Root, which resolves each path component INSIDE
// the kernel and refuses to traverse or land on a symlink escaping root. A
// lexical filepath.Join check cannot do this: the tree we write into is exactly
// the tree the sandbox agent (and any imported repo — git carries symlinks) can
// write to, so `<root>/AGENTS.md -> ~/.bashrc` would otherwise be FOLLOWED and
// truncate an operator file, wardynd running as the operator in host mode. The
// lexical check stays as a cheap first gate against a `..` in a generated key.
//
// workspacescan.EnvAsCodeDockerfilePath is special-cased: every OTHER emitted
// key is Wardyn's own narrow, regenerate-on-demand output (the card's own
// copy promises "regenerate after a rescan or a requirements change" for
// devcontainer.json/AGENTS.md, and the artifact-redirect stubs are one-line
// registry pointers with no plausible hand-authored equivalent) — but
// .devcontainer/Dockerfile is exactly where an operator using devcontainers
// already puts their OWN hand-written Dockerfile, unrelated to Wardyn. A
// pre-existing file there is protected UNLESS its content is byte-identical
// to what Wardyn would write right now — genAgentToolDockerfile is a pure
// function of tools, so that can only be Wardyn's own previously-emitted
// stub, never an operator's coincidence — in which case it is refreshed like
// every other key, not reported skipped. Keying the guard on existence alone
// would make the SECOND "Write into the directory" click always find the
// FIRST click's own stub in the way, permanently closing the regenerate path
// for this one file and falsifying the card's "won't include the agent CLI
// unless you add that yourself" copy. Only a Dockerfile whose content
// actually differs — genuinely the operator's — is left alone and reported.
func writeEnvAsCode(rootPath string, files map[string]string) ([]string, error) {
	cleanRoot := filepath.Clean(rootPath)
	root, err := os.OpenRoot(cleanRoot)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	var skipped []string
	for rel, content := range files {
		dst := filepath.Join(cleanRoot, filepath.FromSlash(rel))
		if !strings.HasPrefix(dst, cleanRoot+string(filepath.Separator)) {
			return nil, fmt.Errorf("refusing to write outside workspace: %s", rel)
		}
		relPath := filepath.FromSlash(rel)
		if dir := filepath.Dir(relPath); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return nil, fmt.Errorf("refusing to write %s: %w", rel, err)
			}
		}
		flag := os.O_WRONLY | os.O_CREATE | os.O_TRUNC
		if rel == workspacescan.EnvAsCodeDockerfilePath {
			// The EXCL guard is needed only when a pre-existing file can't
			// already be PROVEN to be Wardyn's own: byte-identical to what
			// would be written right now (genAgentToolDockerfile is a pure
			// function of tools, so only Wardyn's own previous stub can
			// match). A read error — including "does not exist" — falls
			// through to the guard, the safe default: create fresh, or fail
			// closed into the EEXIST-skip path below rather than guess.
			if existing, rerr := root.ReadFile(relPath); rerr != nil || string(existing) != content {
				flag = os.O_WRONLY | os.O_CREATE | os.O_EXCL
			}
		}
		f, err := root.OpenFile(relPath, flag, 0o644)
		if err != nil {
			if os.IsExist(err) {
				skipped = append(skipped, rel)
				continue
			}
			return nil, fmt.Errorf("refusing to write %s: %w", rel, err)
		}
		_, werr := f.WriteString(content)
		cerr := f.Close()
		if werr != nil {
			return nil, werr
		}
		if cerr != nil {
			return nil, cerr
		}
	}
	slices.Sort(skipped)
	return skipped, nil
}

// handleDeleteWorkspace removes a workspace. Returns 404 when unknown, 204 on success.
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	// bug-workspace-1: read the row's built image ref BEFORE the delete drops
	// the only pointer to it — best-effort (a lookup failure here just means
	// the reclaim is skipped; DeleteWorkspace below still 404s a genuinely
	// missing row on its own).
	var staleImage string
	if ws, gerr := s.cfg.Store.GetWorkspace(r.Context(), id); gerr == nil {
		staleImage = ws.ImageRef
	}
	err := s.cfg.Store.DeleteWorkspace(r.Context(), id)
	if notFoundIf(w, err, "workspace") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete workspace: "+err.Error())
		return
	}
	s.removeStaleImage(r.Context(), staleImage, "")
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.delete", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// handleScanWorkspace scans an onboarded workspace — a fan-out over its
// attached sources, each of which owns its own scan lifecycle (the tier-1
// retarget that fixed the old 3-branch switch's bug: a repo+dir workspace
// never scanned its dirs, because the repo branch won on every call):
//
//   - every attached local_dir scans HOST-SIDE inline (bounded, read-only
//     workspacescan.Scan), landing on the SOURCE row immediately;
//   - every attached repo launches its own governed clone-and-scan run,
//     fenced on that source's active_run_id (202 with scan_run_ids);
//   - ephemeral attachments have nothing to scan — an ephemeral-only
//     composition reads scanned with an empty profile straight from the
//     hydrate pass.
//
// The workspace's profile/status are DERIVED (merge/worst-of its sources) at
// the store's hydrate pass, so the 200 body is the freshly-merged profile.
func (s *Server) handleScanWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	s.scanAttachedSources(w, r, ws)
}
