// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//lint:file-ignore SA1019 This file CONTAINS the compatibility fold: it reads the
// deprecated single-source scalars (Kind/Source/Ref/DefaultTarget/Writable, and
// the container kind) precisely so a pre-composition CLI or SDK caller keeps
// working. Deprecating them is what tells NEW callers to use Sources; the fold
// is the reason they can still be deprecated rather than deleted.

package api

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/hostrules"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
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
		// USABLE ON CREATE. A workspace used to be born pending_scan and a scan
		// run promoted it; the 0.5 dialog does one POST and no scan, so nothing
		// promotes it any more (see migration 0036). Creating it pending_scan
		// meant a permanent "Setting up" chip for work that would never happen,
		// while a run could attach it perfectly well the whole time —
		// resolveCreateRunImage is fail-open by design.
		Status:    types.WorkspaceScanned,
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
				if !hostrules.ValidApprovedHost(d) {
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

// handleSetDeniedEgress replaces the workspace's operator-owned denied-egress
// list (PUT semantics: the body is the FULL list; un-deny by omission — same
// idempotent, no-per-host-delete shape as handleSetApprovedEgress). Backs
// Phase 4's revocation surface (SetWorkspaceDeniedEgress, store.go): this is
// the ONLY way to undo a `deny · always` decision once made — including the
// one H5 warns about, where a deny on a model-provider host permanently
// defeats that workspace's credential injection (deny beats allow at the
// proxy, policy.go), because api.anthropic.com/similar can only ever reach
// AllowedDomains through modelProviderEgress, never through this handler's own
// validator. So this route is also the only cure for an ALREADY-bricked
// workspace, which is exactly why its validator is deliberately narrower than
// handleSetApprovedEgress's:
//
// Do NOT copy handleSetApprovedEgress's deadHosts set here. That set is
// ALLOW-shaped — git-broker/control-plane hosts a real run's proxy never
// consults as a plain ApprovedEgress entry, so promoting one is dead weight —
// and it carries no model-provider guard. Copying it would make this PUT a
// SECOND, unguarded door to H5's brick, on the one route whose entire job is
// to be the escape hatch FROM that brick. So this validates only
// hostrules.ValidApprovedHost (plain lowercase dotted host, no scheme/port/
// wildcard) and nothing else — deliberately unguarded, on purpose, because a
// deny-direction guard here would remove the only way to undo an over-broad
// one.
func (s *Server) handleSetDeniedEgress(w http.ResponseWriter, r *http.Request) {
	type body struct {
		Domains []string `json:"domains"`
	}
	scopedWorkspaceWrite(s, w, r, "workspace.egress.deny",
		func(req body) ([]string, string) {
			if len(req.Domains) > maxApprovedEgress {
				return nil, "too many domains (max 64)"
			}
			set := map[string]struct{}{}
			for _, d := range req.Domains {
				d = strings.ToLower(strings.TrimSpace(d))
				if !hostrules.ValidApprovedHost(d) {
					return nil, "invalid domain (plain lowercase host, no scheme/port/wildcard): " + d
				}
				set[d] = struct{}{}
			}
			return sortedKeys(set), ""
		},
		// Wrapped, not passed as a method value: the store call must not be
		// resolved until validation has passed.
		func(ctx context.Context, id uuid.UUID, doms []string) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceDeniedEgress(ctx, id, doms)
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
			if host == "" || allowed[host] || !hostrules.ValidApprovedHost(host) {
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
