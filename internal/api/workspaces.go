// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
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

// validateWorkspaceLLMCred checks an operator-supplied cred binding (names only;
// the secret's presence is checked at run-create, not here).
func validateWorkspaceLLMCred(c *types.WorkspaceLLMCred) string {
	if c == nil {
		return ""
	}
	switch c.Mode {
	case types.WorkspaceLLMCredNone, types.WorkspaceLLMCredManaged:
		return ""
	case types.WorkspaceLLMCredAPIKey:
		if strings.TrimSpace(c.APIKeySecret) == "" {
			return "llm_cred.api_key_secret is required for mode=api_key"
		}
		// Same reserved-name guard the credential SINKS enforce (sinkReservedSecret:
		// policy.go / inline_policy.go / injection.go): a binding must not name a
		// platform-internal or managed-credential secret — the sink refuses to
		// resolve it, so a run would fail the proxy closed at startup, and it must
		// never be a route to inject wardyn's own signing key / OAuth tokens.
		if sinkReservedSecret(strings.TrimSpace(c.APIKeySecret)) {
			return "llm_cred.api_key_secret must not name a reserved platform-internal secret"
		}
		return ""
	case types.WorkspaceLLMCredBedrock:
		// The per-workspace Bedrock selection overrides the server's global
		// region/model at dispatch (resolveBedrockAuth). Omit the whole block to
		// inherit the global config; set it and you must set BOTH — a half override
		// (this workspace's region against the global model id, or vice versa) is
		// the misconfiguration that 403s at invoke time, since an inference-profile
		// model id is region-scoped. Fail closed at write, mirroring the api_key
		// arm above rather than silently half-applying at dispatch.
		if b := c.Bedrock; b != nil && (strings.TrimSpace(b.Region) == "") != (strings.TrimSpace(b.Model) == "") {
			return "llm_cred.bedrock needs BOTH region and model (or neither — omit them to inherit the server's global Bedrock configuration)"
		}
		return ""
	default:
		return `llm_cred.mode must be "" (none), "managed", "api_key", or "bedrock"`
	}
}

// decodeWorkspaceRequest decodes and validates a workspace request body.
// Unknown JSON fields are rejected (decodeStrictMsg, mirroring
// decodePolicyRequest's typo-safety) and everything is validated before any
// store write — workspaces are admin-gated onboarding config, so a bad source
// must never be persisted (fail closed). It
// requires a non-empty name/source, a recognized kind, and runs the SAME
// safety checks the run-creation path already enforces on the equivalent
// free-text field: local_dir reuses runner.ValidateMountSource, the host
// bind-mount deny-list (onboarding vets the reusable host SOURCE; the
// per-run target is chosen later, per-attach — WorkspaceMount.Target /
// Workspace.DefaultTarget); repo reuses repoFieldSafe + repoCloneURL
// (runs.go), the same pair that gates AgentRun.Repo today. An optional
// DefaultTarget is validated via runner.ValidateTarget for either kind, since
// it becomes an in-container mount/clone target once a run attaches this
// workspace.
func decodeWorkspaceRequest(w http.ResponseWriter, r *http.Request) (workspaceRequest, string) {
	var req workspaceRequest
	if msg := decodeStrictMsg(w, r, &req); msg != "" {
		return workspaceRequest{}, msg
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		return workspaceRequest{}, "name is required"
	}
	req.Source = strings.TrimSpace(req.Source)
	if req.Source == "" {
		return workspaceRequest{}, "source is required"
	}
	switch req.Kind {
	case types.WorkspaceKindLocalDir:
		if err := runner.ValidateMountSource(req.Source); err != nil {
			return workspaceRequest{}, "invalid source: " + err.Error()
		}
	case types.WorkspaceKindRepo:
		if !repoFieldSafe(req.Source) {
			return workspaceRequest{}, "source must not contain control characters or whitespace"
		}
		if repoCloneURL(req.Source) == "" {
			return workspaceRequest{}, "source is not a recognized repo slug or http(s) clone URL"
		}
	case types.WorkspaceKindContainer:
		// A bring-your-own base IMAGE as a named execution environment. Source is
		// the image ref (tag or digest); there is no host mount to validate. Basic
		// shape guard only — the daemon validates the ref for real at pull/wrap time.
		if !repoFieldSafe(req.Source) {
			return workspaceRequest{}, "source (image ref) must not contain control characters or whitespace"
		}
	default:
		return workspaceRequest{}, `kind must be "local_dir", "repo", or "container"`
	}
	if req.DefaultTarget != "" {
		if err := runner.ValidateTarget(req.DefaultTarget); err != nil {
			return workspaceRequest{}, "invalid default_target: " + err.Error()
		}
	}
	if msg := validateWorkspaceLLMCred(req.LLMCred); msg != "" {
		return workspaceRequest{}, msg
	}
	return req, ""
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

// sshWorkspaceSourceReady returns a 400-worthy message when a repo workspace's
// source is an SSH clone URL to a supported provider but the operator has not yet
// stored the canonical ssh-key-<host> secret the clone needs — rejecting at
// onboarding instead of accepting a workspace whose every scan/verify/record clone
// would then fail. "" = fine (not an SSH source, or the secret is present). It runs
// AFTER decodeWorkspaceRequest (which already rejects an SSH URL to an unsupported
// host via repoCloneURL). Shared by create + update so switching an existing
// workspace's source to SSH is covered too.
func (s *Server) sshWorkspaceSourceReady(ctx context.Context, req workspaceRequest) string {
	if req.Kind != types.WorkspaceKindRepo {
		return ""
	}
	host, ok := sshCloneHost(req.Source)
	if !ok {
		return ""
	}
	secretName, ok := canonicalSSHKeySecret(host)
	if !ok {
		return ""
	}
	names, err := s.listUserSecretNames(ctx)
	if err != nil {
		// Don't hard-block onboarding on a transient secret-store read error; the
		// run-time grant path still gates the actual clone.
		return ""
	}
	if slices.Contains(names, secretName) {
		return ""
	}
	return "SSH source needs the " + secretName + " secret first — store your private key via setup's SCM import or `wardyn secret set " + secretName + "`"
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
	if msg := s.sshWorkspaceSourceReady(r.Context(), req); msg != "" {
		writeError(w, http.StatusBadRequest, msg)
		return
	}
	now := s.cfg.Now().UTC()
	id := uuid.New()
	ws := types.Workspace{
		ID:            id,
		Name:          req.Name,
		Kind:          req.Kind,
		Source:        req.Source,
		Ref:           req.Ref,
		DefaultTarget: req.DefaultTarget,
		Writable:      req.Writable,
		LLMCred:       req.LLMCred,
		Status:        types.WorkspacePendingScan,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	created, err := s.cfg.Store.CreateWorkspace(r.Context(), ws)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "create workspace: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.create", id.String(), "success", mustJSON(map[string]any{
			// writable is recorded: it is the operator's consent to let a sandboxed
			// agent's changes persist to a HOST directory, so it belongs in the trail.
			"name": created.Name, "kind": created.Kind, "source": created.Source,
			"writable": created.Writable,
		})))
	writeJSON(w, http.StatusCreated, created)
}

// handleUpdateWorkspace replaces a workspace's editable identity fields (name,
// kind, source, ref, default_target), round-tripping the fetched row so the
// scan-owned fields survive (the store UPDATE replaces every column — the old
// construct-from-scratch call zeroed status, violating its CHECK constraint).
// When SOURCE or KIND changes, the scan state (profile/image/status) AND the
// operator's egress approvals are reset: both were reviewed against the OLD
// content and must be re-earned. Returns 404 when unknown, 400 on an invalid
// body/source.
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
	if msg := s.sshWorkspaceSourceReady(r.Context(), req); msg != "" {
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
	// A repo's Ref change is content-changing too: the profile and approvals
	// were reviewed against the old branch/tag.
	rescan := ws.Source != req.Source || ws.Kind != req.Kind ||
		(req.Kind == types.WorkspaceKindRepo && ws.Ref != req.Ref)
	ws.Name, ws.Kind, ws.Source, ws.Ref, ws.DefaultTarget =
		req.Name, req.Kind, req.Source, req.Ref, req.DefaultTarget
	// writable is an editable identity field too: the store UPDATE replaces every
	// column, so omitting it here would silently REVOKE a granted write opt-in on
	// any unrelated edit (rename, retarget).
	ws.Writable = req.Writable
	if rescan {
		ws.Profile = nil
		ws.ImageRef = ""
		ws.BuiltProfileHash = ""
		ws.ApprovedEgress = nil
		// Operator approvals and recorded evidence were reviewed against the OLD
		// content too: stale setup commands must not auto-run against new source,
		// and stale verify/record results — including the verified_* "this image
		// was PROVEN to install/build/test" stamp — must not read as proof for it.
		ws.SetupCommands = nil
		ws.VerifyResult = nil
		ws.RecordResults = nil
		ws.VerifiedProfileHash = ""
		ws.VerifiedAt = nil
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
			"name": updated.Name, "kind": updated.Kind, "source": updated.Source, "rescan_required": rescan,
		})))
	writeJSON(w, http.StatusOK, updated)
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
			if req.Mode == types.WorkspaceLLMCredNone {
				return nil, "" // nil clears the binding
			}
			return &req, ""
		},
		func(ctx context.Context, id uuid.UUID, cred *types.WorkspaceLLMCred) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceLLMCred(ctx, id, cred)
		},
		func(cred *types.WorkspaceLLMCred) map[string]any {
			mode := types.WorkspaceLLMCredNone
			if cred != nil {
				mode = cred.Mode
			}
			return map[string]any{"mode": string(mode)}
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
// slug/URL). Only the PRIMARY workspace is linked on the run, so observed
// telemetry is scoped to runs where ws was primary — a deliberate,
// honest limit (secondary mounts aren't denormalized onto the run).
func runUsesWorkspace(run types.AgentRun, ws types.Workspace) bool {
	if ws.Kind == types.WorkspaceKindLocalDir {
		return run.WorkspacePath == ws.Source
	}
	return run.Repo == ws.Source
}

// maxSetupCommands bounds the operator-approved setup-command list.
const maxSetupCommands = 32

// handleSetSetupCommands replaces the workspace's operator-APPROVED setup
// commands (install/build/test/lint) that a verify run will execute. Full
// replacement (PUT), audited, stored OUTSIDE the scan-owned profile blob — like
// approved-egress, a detected command is advisory until the operator promotes
// it here, and a rescan can neither add nor resurrect an approval. These
// commands RUN (confined) at verify time, so each is validated to a single-line,
// bounded, control-char-free string, and only known stages are accepted.
func (s *Server) handleSetSetupCommands(w http.ResponseWriter, r *http.Request) {
	type body struct {
		Commands []workspacescan.SetupCommand `json:"commands"`
	}
	scopedWorkspaceWrite(s, w, r, "workspace.import.setup_commands",
		func(req body) ([]workspacescan.SetupCommand, string) {
			if len(req.Commands) > maxSetupCommands {
				return nil, "too many commands (max 32)"
			}
			for _, c := range req.Commands {
				if !workspacescan.ValidSetupCommand(c) {
					return nil, "invalid setup command (stage must be install|build|test|lint; command single-line ≤512 chars)"
				}
			}
			return req.Commands, ""
		},
		func(ctx context.Context, id uuid.UUID, cmds []workspacescan.SetupCommand) (types.Workspace, error) {
			return s.cfg.Store.SetWorkspaceSetupCommands(ctx, id, mustJSON(cmds)) // canonical stored form
		},
		func(cmds []workspacescan.SetupCommand) map[string]any {
			// Counts + stages only in audit — never anything value-shaped.
			stages := make([]string, 0, len(cmds))
			for _, c := range cmds {
				stages = append(stages, c.Stage)
			}
			return map[string]any{"count": len(cmds), "stages": stages}
		})
}

// handleVerifyWorkspace launches a governed VERIFY run that executes the
// workspace's operator-approved SetupCommands (install/build/test) in its built
// devcontainer image under confinement, and reports per-step results back. The
// workspace flips to `verifying`; the verify-result upload flips it to `ready`
// (green) or `verify_failed`. Requires approved setup commands + a runner —
// under -runner none it honestly 503s ("verify needs a runner") rather than
// claiming ready without running anything.
func (s *Server) handleVerifyWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}
	if len(ws.SetupCommands) == 0 || string(ws.SetupCommands) == "null" {
		writeError(w, http.StatusUnprocessableEntity, "no approved setup commands — approve them via PUT /workspaces/{id}/setup-commands first")
		return
	}
	if s.cfg.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "verify needs a configured runner (this control plane runs with -runner none; scan and configure still work)")
		return
	}
	// A stale active_run_id (its run failed to upload, was killed, or idle-reaped)
	// must not permanently 409-brick re-verify: only block when the pointed-to
	// run is genuinely still live.
	if ws.ActiveRunID != nil {
		if active, gerr := s.cfg.Store.GetRun(r.Context(), *ws.ActiveRunID); gerr == nil && !isTerminalRunState(active.State) {
			writeError(w, http.StatusConflict, "an import step is already running for this workspace")
			return
		}
	}

	// launchVerifyRun flips the workspace to `verifying` + claims the in-flight run
	// pointer BEFORE it dispatches — like the scan/record lanes — so a fast
	// verify whose result upload lands immediately isn't regressed by a status write
	// that arrives after it. No post-launch state write here.
	actorType, actor := actorFromRequest(r)
	run, lerr := s.launchVerifyRun(r.Context(), actor, ws, ws.SetupCommands)
	if errors.Is(lerr, errImportStepBusy) {
		// Lost the serial-slot CAS to a run launched between our liveness check and
		// the claim (M1) — surface as a clean 409, not a 500.
		writeError(w, http.StatusConflict, "an import step is already running for this workspace")
		return
	}
	if lerr != nil {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
			"workspace.import.verify", id.String(), "failure", mustJSON(map[string]any{"detail": lerr.Error()})))
		writeError(w, http.StatusInternalServerError, "launch verify run: "+lerr.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor,
		"workspace.import.verify", id.String(), "success", mustJSON(map[string]any{"verify_run_id": run.ID.String()})))
	writeJSON(w, http.StatusAccepted, map[string]any{
		"verify_run_id": run.ID, "workspace_id": id, "state": run.State,
		"detail": "governed verify run launched; the workspace status updates when it completes",
	})
}

// handleFinalizeWorkspace completes an import: marks the workspace ready and,
// when the operator opts in, EMITS committable env-as-code (a devcontainer.json
// + AGENTS.md from the verified profile + approved setup commands) — the detect-
// AND-emit differentiator. For a local_dir workspace the files are written into
// the host source dir (operator-confirmed, fixed safe paths); for a repo they
// are returned in the response for the operator to commit.
func (s *Server) handleFinalizeWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	var req struct {
		EmitEnvAsCode bool `json:"emit_env_as_code"`
	}
	if r.ContentLength != 0 {
		if !decodeStrict(w, r, &req) {
			return
		}
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}

	// Refuse to finalize while an import step is still LIVE: finalize zeroes
	// active_run_id and marks the workspace ready, which would silently drop the
	// running verify/record run's real result — its later result upload then 409s
	// on the now-cleared pointer. Mirror the same live-run guard verify/record use
	// (a stale pointer to a terminal run does not block — only a genuinely live one).
	if ws.ActiveRunID != nil {
		if active, gerr := s.cfg.Store.GetRun(r.Context(), *ws.ActiveRunID); gerr == nil && !isTerminalRunState(active.State) {
			writeError(w, http.StatusConflict, "an import step is still running for this workspace; wait for it to finish before finalizing")
			return
		}
	}

	emitted := map[string]string{}
	if req.EmitEnvAsCode {
		files, ok := s.envAsCodeFor(w, r, ws)
		if !ok {
			return
		}
		if ws.Kind == types.WorkspaceKindLocalDir {
			if werr := writeEnvAsCode(ws.Source, files); werr != nil {
				writeError(w, http.StatusInternalServerError, "write env-as-code: "+werr.Error())
				return
			}
		} else {
			// Repo: return content for the operator to commit (a broker-driven
			// branch-commit is a follow-up). Dismissing the dialog does not lose
			// it — GET /workspaces/{id}/env-as-code regenerates the same files.
			emitted = files
		}
	}

	// Mark ready. If a verify already passed, status is already ready; otherwise
	// finalize confirms a configured (verify-skipped) import as ready.
	//
	// FENCED on the slot the active-run guard above read: that guard only proves
	// no run was live AT READ TIME, so a verify/record run claiming the slot
	// between the check and this write would otherwise be silently clobbered by
	// this finalize (the guard closes the window it can see; the fence closes the
	// rest of it).
	updated, applied, err := s.cfg.Store.SetWorkspaceImportState(r.Context(), id, types.WorkspaceReady,
		nil, ws.ActiveRunID, ws.VerifyResult, ws.VerifiedProfileHash, ws.VerifiedAt)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "finalize: "+err.Error())
		return
	}
	if !applied {
		writeError(w, http.StatusConflict, "an import step claimed this workspace while finalizing; re-check its state and retry")
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"workspace.import.finalize", id.String(), "success", mustJSON(map[string]any{
			"emitted": req.EmitEnvAsCode, "files": len(emitted),
		})))
	writeJSON(w, http.StatusOK, map[string]any{"workspace": updated, "emitted_files": emitted})
}

// envAsCodeFor generates the committable env-as-code for a workspace from its
// CURRENT reviewed profile + approved setup commands. Shared by finalize and
// handleGetEnvAsCode so the two generations can never drift. It writes its own
// error response (422 when the workspace has no scanned profile, 500 when the
// generator fails) and returns ok=false, mirroring getWorkspaceOr404.
func (s *Server) envAsCodeFor(w http.ResponseWriter, r *http.Request, ws types.Workspace) (map[string]string, bool) {
	profile, ok := workspaceProfile(ws)
	if !ok {
		writeError(w, http.StatusUnprocessableEntity, "workspace has no scanned profile to emit from")
		return nil, false
	}
	var approved []workspacescan.SetupCommand
	_ = json.Unmarshal(ws.SetupCommands, &approved)
	// Fold the operator-wide artifact-registry redirects (URL-only) into the
	// committable output so an exported workspace pulls from the corp mirror.
	// Best-effort: a store error / no site-config just omits them.
	var artifactBases map[string]string
	if s.cfg.Store != nil {
		if sc, scErr := s.cfg.Store.GetSiteConfig(r.Context()); scErr == nil {
			artifactBases = artifactBaseURLs(sc)
		}
	}
	files, gerr := workspacescan.EmitEnvAsCode(profile, approved, artifactBases)
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
//   - local_dir: scanned HOST-SIDE inline via workspacescan.Scan (bounded,
//     read-only, no subprocess — the host control plane can read the reusable
//     onboarded path directly). The derived profile is persisted, status flips
//     to scanned (NOT ready — `ready` means the import was finalized/verified)
//     and the profile is returned (200). An onboarded path that is gone or is
//     not a directory persists status=error and 422s instead, so a typo never
//     reads as green.
//   - repo: a repo is NOT on the host — it scans as a governed throwaway run
//     whose ScanFacts return over the brokered scan-result route
//     (handleUploadScanResult). Launching it returns 202 with the scan_run_id
//     (503 with no runner configured, 409 when an import step already holds the
//     workspace's slot); the profile lands asynchronously.
func (s *Server) handleScanWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}

	switch ws.Kind {
	case types.WorkspaceKindLocalDir:
		// A nonexistent / unreadable path must NOT report green "Ready": Scan() never
		// errors (it degrades to a low-confidence profile on a bound/unknown build
		// system), so an operator typo would otherwise flip straight to Ready and only
		// surface much later as an empty sandbox mount. Stat the source first and
		// persist status=error with an actionable reason instead.
		if fi, serr := os.Stat(ws.Source); serr != nil || !fi.IsDir() {
			detail := "local directory not found on this host: " + ws.Source
			if serr == nil && !fi.IsDir() {
				detail = "onboarded source is not a directory: " + ws.Source
			}
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
		profile := workspacescan.Scan(ws.Source)
		ws.Profile = mustJSON(profile)
		// Scanned, not ready: the import flow continues (configure → verify →
		// finalize). A scanned workspace is already usable for runs (the mount
		// gate is onboarding-based, not status-based); `ready` now means the
		// import was finalized/verified.
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
				"kind": ws.Kind, "confidence": profile.Confidence, "needs_review": profile.NeedsReview,
				"secret_reqs": len(profile.RequiredSecrets), "services": len(profile.ServicesNeeded),
				"suggested_egress": len(profile.SuggestedEgress), "secret_files": len(profile.SecretFilesPresent),
				"leak_findings": len(profile.LeakFindings), "build_mem_mib": profile.BuildMemoryMiB,
			})))
		writeJSON(w, http.StatusOK, profile)
	case types.WorkspaceKindRepo:
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
				"kind": ws.Kind, "scan_run_id": run.ID.String(),
			})))
		// 202: the profile is populated asynchronously when the scan run uploads its
		// ScanFacts (SetWorkspaceScanResult flips the workspace to status=scanned).
		writeJSON(w, http.StatusAccepted, map[string]any{
			"scan_run_id": run.ID, "workspace_id": ws.ID, "state": run.State,
			"detail": "governed scan run launched; the workspace profile updates when the scan completes",
		})
	default:
		writeError(w, http.StatusBadRequest, "unknown workspace kind")
	}
}
