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
	"errors"
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

	for i, src := range req.Sources {
		if msg := validateWorkspaceSource(src); msg != "" {
			return workspaceRequest{}, fmt.Sprintf("sources[%d]: %s", i, msg)
		}
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
			// Don't hard-block onboarding on a transient secret-store read error;
			// the run-time grant path still gates the actual clone.
			names, _ = s.listUserSecretNames(ctx)
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
		BaseImage: req.BaseImage,
		LLMCred:   req.LLMCred,
		Status:    types.WorkspacePendingScan,
		CreatedAt: now,
		UpdatedAt: now,
	}
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
	rescan := !slices.Equal(ws.Sources, req.Sources) || !baseImageEqual(ws.BaseImage, req.BaseImage)
	ws.Name, ws.Sources, ws.BaseImage = req.Name, req.Sources, req.BaseImage
	if rescan {
		ws.Profile = nil
		ws.ImageRef = ""
		ws.BuiltProfileHash = ""
		ws.ApprovedEgress = nil
		// Operator approvals and recorded evidence were reviewed against the OLD
		// content too: stale requirements/record results must not read as proof
		// for the new composition.
		ws.Requirements = nil
		ws.RecordResults = nil
		ws.Status = types.WorkspacePendingScan
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
			"name": updated.Name, "sources": len(updated.Sources), "rescan_required": rescan,
		})))
	writeJSON(w, http.StatusOK, updated)
}

// baseImageEqual reports whether two base-image choices are equivalent
// (nil-safe; Steps compared by content).
func baseImageEqual(a, b *types.WorkspaceBaseImage) bool {
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
	typ, rest, found := strings.Cut(key, ":")
	if !found || rest == "" {
		return "", "", false
	}
	switch typ {
	case "secret", "egress", "write", "integration":
		return typ, rest, true
	default:
		return "", "", false
	}
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
		if !validSecretRef(rest) {
			return fmt.Sprintf("requirement %q: invalid secret name", key)
		}
	case "integration":
		// Shape only — an integration id is the same identifier secret names
		// use (validateIntegrationWrite). EXISTENCE is deliberately not checked
		// here: a workspace may name an integration before it is configured
		// (the contract states an intent), and the fold degrades silently to
		// "opens nothing" until the row exists. Requiring it to exist first
		// would make ordering the operator's problem.
		if !secretNameRE.MatchString(rest) {
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
			return s.cfg.Store.SetWorkspaceRequirements(ctx, id, reqs)
		},
		func(reqs map[string]types.WorkspaceRequirement) map[string]any {
			return map[string]any{"count": len(reqs)}
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
	if werr := writeEnvAsCode(localDirs[0].Path, files); werr != nil {
		writeError(w, http.StatusInternalServerError, "write env-as-code: "+werr.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.envcode.write", id.String(), "success", mustJSON(map[string]any{"files": len(files)})))
	writeJSON(w, http.StatusOK, map[string]any{"written_files": files})
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
	files, gerr := workspacescan.EmitEnvAsCode(profile, artifactBases)
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

// writeEnvAsCode writes generated env-as-code files under root. Paths are the
// fixed, safe outputs of EmitEnvAsCode (.devcontainer/devcontainer.json,
// AGENTS.md, plus any artifact-redirect config like .npmrc/.cargo/config.toml).
//
// Every write goes through os.Root, which resolves each path component INSIDE
// the kernel and refuses to traverse or land on a symlink escaping root. A
// lexical filepath.Join check cannot do this: the tree we write into is exactly
// the tree the sandbox agent (and any imported repo — git carries symlinks) can
// write to, so `<root>/AGENTS.md -> ~/.bashrc` would otherwise be FOLLOWED and
// truncate an operator file, wardynd running as the operator in host mode. The
// lexical check stays as a cheap first gate against a `..` in a generated key.
func writeEnvAsCode(rootPath string, files map[string]string) error {
	cleanRoot := filepath.Clean(rootPath)
	root, err := os.OpenRoot(cleanRoot)
	if err != nil {
		return err
	}
	defer root.Close()
	for rel, content := range files {
		dst := filepath.Join(cleanRoot, filepath.FromSlash(rel))
		if !strings.HasPrefix(dst, cleanRoot+string(filepath.Separator)) {
			return fmt.Errorf("refusing to write outside workspace: %s", rel)
		}
		relPath := filepath.FromSlash(rel)
		if dir := filepath.Dir(relPath); dir != "." {
			if err := root.MkdirAll(dir, 0o755); err != nil {
				return fmt.Errorf("refusing to write %s: %w", rel, err)
			}
		}
		f, err := root.OpenFile(relPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
		if err != nil {
			return fmt.Errorf("refusing to write %s: %w", rel, err)
		}
		_, werr := f.WriteString(content)
		cerr := f.Close()
		if werr != nil {
			return werr
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// handleDeleteWorkspace removes a workspace. Returns 404 when unknown, 204 on success.
func (s *Server) handleDeleteWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	err := s.cfg.Store.DeleteWorkspace(r.Context(), id)
	if notFoundIf(w, err, "workspace") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "delete workspace: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.delete", id.String(), "success", nil))
	w.WriteHeader(http.StatusNoContent)
}

// handleScanWorkspace scans an onboarded workspace and persists its profile.
//
//   - a REPO source needs a governed clone-and-scan run — there is no host-side
//     way to scan it. It scans as a governed throwaway run whose ScanFacts
//     return over the brokered scan-result route (handleUploadScanResult).
//     Launching it returns 202 with the scan_run_id (503 with no runner
//     configured, 409 when an import step already holds the workspace's
//     slot); the profile lands asynchronously.
//     ponytail: when a repo source is present, this scans ONLY the first one
//     (firstRepoSource) and any local_dir sources on the SAME workspace are
//     scanned by a LATER call once the repo scan lands, not merged in the
//     same pass — multi-source aggregation across BOTH kinds in one governed
//     run is the upgrade path if that's ever needed.
//   - otherwise, every local_dir source is scanned HOST-SIDE inline via
//     workspacescan.Scan (bounded, read-only, no subprocess — the host control
//     plane can read the reusable onboarded path directly) and merged into one
//     profile (mergeWorkspaceProfiles). The derived profile is persisted,
//     status flips to scanned, and the profile is returned (200). An onboarded
//     path that is gone or is not a directory persists status=error and 422s
//     instead, so a typo never reads as green.
//   - an ephemeral-only composition has nothing to scan: it is marked scanned
//     with an empty profile immediately (a scratch dir implies nothing about
//     languages/egress/secrets).
func (s *Server) handleScanWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}

	switch localDirs := workspaceSourcesOfType(ws, types.WorkspaceSourceTypeLocalDir); {
	case len(workspaceSourcesOfType(ws, types.WorkspaceSourceTypeRepo)) > 0:
		if s.cfg.Runner == nil {
			writeError(w, http.StatusServiceUnavailable, "no runner configured to launch a governed scan run")
			return
		}
		actorType, actor := actorFromRequest(r)
		run, lerr := s.launchScanRun(r.Context(), actor, ws)
		if errors.Is(lerr, errImportStepBusy) {
			writeError(w, http.StatusConflict, "an import step is already running for this workspace")
			return
		}
		if lerr != nil {
			s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
				"workspace.scan", id.String(), "failure", mustJSON(map[string]any{"detail": lerr.Error()})))
			writeError(w, http.StatusInternalServerError, "launch scan run: "+lerr.Error())
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor,
			"workspace.scan", id.String(), "success", mustJSON(map[string]any{
				"sources": len(ws.Sources), "scan_run_id": run.ID.String(),
			})))
		// 202: the profile is populated asynchronously when the scan run uploads its
		// ScanFacts (SetWorkspaceScanResult flips the workspace to status=scanned).
		writeJSON(w, http.StatusAccepted, map[string]any{
			"scan_run_id": run.ID, "workspace_id": ws.ID, "state": run.State,
			"detail": "governed scan run launched; the workspace profile updates when the scan completes",
		})
	case len(localDirs) > 0:
		// A nonexistent / unreadable path must NOT report green "Ready": Scan() never
		// errors (it degrades to a low-confidence profile on a bound/unknown build
		// system), so an operator typo would otherwise flip straight to Ready and only
		// surface much later as an empty sandbox mount. Stat every source first and
		// persist status=error with an actionable reason instead.
		profiles := make([]workspacescan.WorkspaceProfile, 0, len(localDirs))
		for _, src := range localDirs {
			fi, serr := os.Stat(src.Path)
			if serr == nil && fi.IsDir() {
				profiles = append(profiles, workspacescan.Scan(src.Path))
				continue
			}
			detail := localDirScanFailureDetail(src.Path, serr == nil && !fi.IsDir(),
				os.Getenv("WARDYN_WORKSPACES_ROOT"), runningInContainer())
			ws.Status = types.WorkspaceError
			if _, uerr := s.cfg.Store.UpdateWorkspace(r.Context(), id, ws); uerr != nil {
				writeError(w, http.StatusInternalServerError, "persist scan status: "+uerr.Error())
				return
			}
			s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"workspace.scan", id.String(), "failure", mustJSON(map[string]any{"detail": detail})))
			writeError(w, http.StatusUnprocessableEntity, detail)
			return
		}
		profile := mergeWorkspaceProfiles(profiles)
		ws.Profile = mustJSON(profile)
		// Scanned: the workspace is already usable for runs (the mount gate is
		// onboarding-based, not status-based).
		ws.Status = types.WorkspaceScanned
		if _, uerr := s.cfg.Store.UpdateWorkspace(r.Context(), id, ws); uerr != nil {
			s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"workspace.scan", id.String(), "failure", mustJSON(map[string]any{"detail": uerr.Error()})))
			writeError(w, http.StatusInternalServerError, "persist scan profile: "+uerr.Error())
			return
		}
		// Counts only — never detected names (and never values) in audit data.
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"workspace.scan", id.String(), "success", mustJSON(map[string]any{
				"local_dir_sources": len(localDirs), "confidence": profile.Confidence, "needs_review": profile.NeedsReview,
				"secret_reqs": len(profile.RequiredSecrets), "services": len(profile.ServicesNeeded),
				"suggested_egress": len(profile.SuggestedEgress), "secret_files": len(profile.SecretFilesPresent),
				"leak_findings": len(profile.LeakFindings), "build_mem_mib": profile.BuildMemoryMiB,
			})))
		writeJSON(w, http.StatusOK, profile)
	default:
		// Ephemeral-only composition: nothing to scan. Mark it scanned with an
		// empty (high-confidence — there is nothing ambiguous about "no source")
		// profile so the workspace is immediately usable.
		profile := workspacescan.WorkspaceProfile{Confidence: workspacescan.ConfidenceHigh, Source: workspacescan.SourceDeterministic}
		ws.Profile = mustJSON(profile)
		ws.Status = types.WorkspaceScanned
		if _, uerr := s.cfg.Store.UpdateWorkspace(r.Context(), id, ws); uerr != nil {
			writeError(w, http.StatusInternalServerError, "persist scan profile: "+uerr.Error())
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"workspace.scan", id.String(), "success", mustJSON(map[string]any{"ephemeral_only": true})))
		writeJSON(w, http.StatusOK, profile)
	}
}
