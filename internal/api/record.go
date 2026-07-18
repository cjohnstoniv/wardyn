// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Record Mode for workspace import: per-task OPEN recording sandboxes.
// The import pipeline is Source → Scan → Configure → Record (recommended,
// skippable) → Verify → Finalize: Record LEARNS what a task actually uses
// (an allow-all-egress run whose audit events are captured server-side via
// recordmode.Capture), the operator PROMOTES observed needs (egress → the
// existing ApprovedEgress lane), and Verify re-runs the same commands
// CONFINED to PROVE least-privilege — verifyEgressDomains already unions
// ApprovedEgress, so promotion widens the subsequent verify with zero extra
// wiring.
//
// State model: no new WorkspaceStatus (record is per-task and skippable; the
// workspace stays `scanned` throughout). Per-task state lives in the opaque
// workspaces.record_results JSONB map, written only via the scoped
// SetWorkspaceRecordResults; serial concurrency rides active_run_id exactly
// like verify (one import step at a time — per-task parallelism is the named
// upgrade path, not built).

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	neturl "net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/recordmode"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// Record task result statuses (task-scoped — NOT WorkspaceStatus values).
const (
	recordStatusRecording = "recording"
	recordStatusRecorded  = "recorded"
	recordStatusFailed    = "record_failed"
)

// recordModeInteractive is the only record execution mode today (persisted on
// RecordTaskResult.Mode); an unattended mode would add its own value.
const recordModeInteractive = "interactive"

// errImportStepBusy is returned when the serial import-step CAS claim loses —
// another step (scan/verify/record) concurrently owns the workspace's slot.
var errImportStepBusy = errors.New("an import step is already running for this workspace")

// recordInteractiveIdleCap bounds an interactive recording's idle lifetime.
// Generous (a human is driving it), but FINITE: an abandoned recording is an
// OPEN-egress sandbox, and it must self-terminate + revoke rather than live
// forever. Attach activity touches the run, so an active session isn't reaped.
const recordInteractiveIdleCap = 4 * time.Hour

// recordMaskingCaveat is the standing honesty note stamped on every capture:
// the secretmask registry is seed-ahead only, so an open run can touch secrets
// nobody declared — those are NOT masked anywhere.
const recordMaskingCaveat = "secret masking is seed-ahead only: a secret this open run touched that was " +
	"never declared to Wardyn is NOT masked in logs or observations — treat raw output as sensitive"

// RecordTaskResult is one task's Record Mode state, persisted opaquely in
// workspaces.record_results (map taskKey → RecordTaskResult). The api layer
// owns this shape; the store never interprets it.
type RecordTaskResult struct {
	RunID uuid.UUID `json:"run_id"`
	// Label is the operator-chosen session name (e.g. "build & test"). Persisted
	// because sessions are user-named, not derived — the session key is a slug of
	// this, so the label carries the original display text.
	Label string `json:"label,omitempty"`
	Mode  string `json:"mode"` // auto | interactive
	// Confined distinguishes a VERIFY session (default-deny egress, limited to the
	// workspace's approved set + baseline) from a learning session (open egress).
	// Same interactive attach machinery; the flag flips AllowAllEgress and lets the
	// UI list learning sessions on the Record step and verify sessions on Verify.
	Confined bool `json:"confined,omitempty"`
	// LLMMode + Model record the auth the session actually ran with (the operator's
	// configured provider): subscription | api-key | none, plus the pinned model.
	// Saved with the session so it's visible and a verify replays the SAME auth as
	// the recording (the operator's setup, not a re-derived guess).
	LLMMode    string     `json:"llm_mode,omitempty"`
	Model      string     `json:"model,omitempty"`
	Status     string     `json:"status"` // recording | recorded | record_failed
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
	// Observations is the deterministic recordmode.Capture aggregate of what the
	// run ACTUALLY used, computed server-side from its audit events at
	// termination — never from a sandbox upload.
	Observations *recordmode.Observations `json:"observations,omitempty"`
	// SecretNamesMinted resolves Observations.MintedGrantIDs to the secret /
	// grant names actually exercised, for the "proven used" checklist render.
	SecretNamesMinted []string `json:"secret_names_minted,omitempty"`
	// EgressPromoted marks that this task's observed hosts were merged into the
	// workspace's ApprovedEgress (operator action, never automatic).
	EgressPromoted bool `json:"egress_promoted,omitempty"`
	// KernelSensorBlind: the run executed under CC3/Kata where the host eBPF
	// sensor cannot see — proxy decisions were the sole egress signal.
	KernelSensorBlind bool `json:"kernel_sensor_blind,omitempty"`
	// FailureHint explains a record_failed in operator terms (e.g. the sandbox
	// couldn't reach the control plane, so no evidence landed).
	FailureHint string   `json:"failure_hint,omitempty"`
	Caveats     []string `json:"caveats,omitempty"`
}

// recordResultsMap decodes the workspace's opaque record_results blob. A
// missing/malformed blob is an empty map (fail-open to "nothing recorded").
func recordResultsMap(ws types.Workspace) map[string]RecordTaskResult {
	out := map[string]RecordTaskResult{}
	if len(ws.RecordResults) > 0 {
		_ = json.Unmarshal(ws.RecordResults, &out)
	}
	return out
}

// putRecordResult upserts one task's entry via the store's ATOMIC per-key
// jsonb merge (never a whole-map read-modify-write, so writers of different
// tasks can't lose each other). onlyIfStatus, when non-empty, makes the write
// a compare-and-set on the entry's CURRENT status — the guard that stops a
// late streaming upload from reverting a completed capture, and makes capture
// itself idempotent across the watcher/kill/boot/read-repair triggers.
func (s *Server) putRecordResult(ctx context.Context, wsID uuid.UUID, taskKey string, res RecordTaskResult, onlyIfStatus string) (types.Workspace, bool, error) {
	return s.cfg.Store.SetWorkspaceRecordResult(ctx, wsID, taskKey, mustJSON(res), onlyIfStatus)
}

// recordVerifyKeyPrefix namespaces a confined verify run's record_results entry so
// it never clobbers the open recording it replays. The ":" cannot appear in a slug.
const recordVerifyKeyPrefix = "verify:"

// recordSessionKeyRE collapses any run of non-[a-z0-9] into a single dash.
var recordSessionKeyRE = regexp.MustCompile(`[^a-z0-9]+`)

// recordSessionKey slugs an operator-chosen session name into a stable map key
// for record_results. Sessions are user-named (not derived from a taxonomy), so
// the key is a sanitized slug of the name and the raw name rides along as Label.
// Returns "" for a name with no usable characters (the caller 400s).
func recordSessionKey(name string) string {
	s := recordSessionKeyRE.ReplaceAllString(strings.ToLower(strings.TrimSpace(name)), "-")
	s = strings.Trim(s, "-")
	if len(s) > 48 {
		s = strings.Trim(s[:48], "-")
	}
	return s
}

// repairStaleWorkspaceRuns settles any import run stranded by a terminal-transition
// path with no reconcile hook — the idle reaper, a watcher lost to a crash, or a
// dispatch-time failure racing a wardynd crash before settleTerminalLaunch ran.
// Two strand shapes: (a) a task entry still `recording` whose run has terminated,
// and (b) a workspace stuck `verifying`/`scanning` whose active run has terminated
// (— record already self-healed on read; verify/scan did not). Cheap +
// race-free: the reconcilers are CAS-guarded and no-op unless a strand is real.
// Returns the (possibly refreshed) row.
func (s *Server) repairStaleWorkspaceRuns(ctx context.Context, ws types.Workspace) types.Workspace {
	repaired := false
	for _, res := range recordResultsMap(ws) {
		if res.Status != recordStatusRecording {
			continue
		}
		if run, err := s.cfg.Store.GetRun(ctx, res.RunID); err == nil && isTerminalRunState(run.State) {
			s.reconcileRecordRun(ctx, res.RunID)
			repaired = true
		}
	}
	// Verify/scan strand: the workspace is mid-import but its in-flight run has
	// already terminated — settle it to a clear failure so the operator sees a
	// reason instead of an endless spinner. reconcileWorkspaceRun re-fences on the
	// active_run_id, so a newer run that has taken the slot is left untouched.
	if ws.ActiveRunID != nil && (ws.Status == types.WorkspaceVerifying || ws.Status == types.WorkspaceScanning) {
		if run, err := s.cfg.Store.GetRun(ctx, *ws.ActiveRunID); err == nil && isTerminalRunState(run.State) {
			s.reconcileWorkspaceRun(ctx, *ws.ActiveRunID)
			repaired = true
		}
	}
	if repaired {
		if fresh, err := s.cfg.Store.GetWorkspace(ctx, ws.ID); err == nil {
			return fresh
		}
	}
	return ws
}

// handleRecordWorkspace launches one named OPEN recording sandbox:
// POST /workspaces/{id}/record {"name": "...", "confined": false}
// ("task_key" is accepted as a deprecated alias for name). The operator drives
// the session through the attach terminal and finishes it with the normal run
// kill ("Done recording"); confined=true replays under the approved egress set.
// 202 with the run id; 503 no runner; 409 while another import step is live.
func (s *Server) handleRecordWorkspace(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	// Named recording sessions: the operator names a session ("build & test",
	// "agent dev loop", …) and drives it in an attached terminal; there is no
	// derived build/test/contribute taxonomy. Back-compat: accept the legacy
	// task_key as an alias for name so old clients don't hard-break.
	var req struct {
		Name    string `json:"name"`
		TaskKey string `json:"task_key"` // deprecated alias for name
		// Confined = a VERIFY session: default-deny egress limited to the approved
		// set, to re-run the same steps under least privilege. Default false = a
		// learning session with open egress (the Record step).
		Confined bool `json:"confined"`
	}
	if !decodeStrict(w, r, &req) {
		return
	}
	ws, ok := s.getWorkspaceOr404(w, r, id)
	if !ok {
		return
	}

	label := strings.TrimSpace(req.Name)
	if label == "" {
		label = strings.TrimSpace(req.TaskKey)
	}
	key := recordSessionKey(label)
	if key == "" {
		writeError(w, http.StatusBadRequest, "record session needs a name (letters/digits) — e.g. \"build & test\"")
		return
	}
	// A verify (confined) run REPLAYS an existing recording under least privilege —
	// it must not clobber that recording's open-mode capture, so it lives under a
	// distinct, derived key ("verify:" + the recording's key). The ":" can't occur
	// in a slug, so this never collides with a learning session's key.
	if req.Confined {
		key = recordVerifyKeyPrefix + key
	}
	// Sessions are interactive: the operator drives the real activity in the attach
	// shell (build, test, run the agent) and stops the run to capture.
	mode := recordModeInteractive
	if s.cfg.Runner == nil {
		writeError(w, http.StatusServiceUnavailable, "record needs a configured runner (this control plane runs with -runner none; scan and configure still work)")
		return
	}
	// A stale active_run_id (its run failed to upload, was killed, or idle-reaped)
	// must not permanently 409-brick recording: only block on a genuinely live run.
	if ws.ActiveRunID != nil {
		if active, gerr := s.cfg.Store.GetRun(r.Context(), *ws.ActiveRunID); gerr == nil && !isTerminalRunState(active.State) {
			writeError(w, http.StatusConflict, "an import step is already running for this workspace")
			return
		}
	}

	actorType, actor := actorFromRequest(r)
	run, weakCC, lerr := s.launchRecordRun(r.Context(), actor, ws, key, label, mode, req.Confined)
	if errors.Is(lerr, errImportStepBusy) {
		writeError(w, http.StatusConflict, "an import step is already running for this workspace")
		return
	}
	if lerr != nil {
		s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor,
			"run.record.start", id.String(), "failure", mustJSON(map[string]any{"task": key, "detail": lerr.Error()})))
		writeError(w, http.StatusInternalServerError, "launch record run: "+lerr.Error())
		return
	}

	auditData := map[string]any{
		"workspace_id": id.String(), "task": key, "label": label, "mode": mode,
		"confinement": string(run.ConfinementClass), "allow_all_egress": !req.Confined,
		"confined": req.Confined,
	}
	if weakCC {
		auditData["weak_confinement"] = "cc1"
	}
	s.recordAudit(r.Context(), s.auditEvent(&run.ID, actorType, actor,
		"run.record.start", id.String(), "success", mustJSON(auditData)))

	detail := "open recording session launched; attach via GET /runs/{id}/attach, do the real activity (build, test, run the agent), then stop the run (Done recording) to capture"
	if req.Confined {
		detail = "confined verify session launched (default-deny egress, limited to the approved set); attach via GET /runs/{id}/attach, re-run the same steps — off-policy hosts are denied live — then stop the run"
	}
	resp := map[string]any{
		"record_run_id": run.ID, "workspace_id": id, "task_key": key, "label": label,
		"mode": mode, "confined": req.Confined, "state": run.State,
		"detail": detail,
	}
	var warnings []string
	// The open-egress exfiltration warning applies only to a learning session; a
	// confined verify session is default-deny, so it carries no such window.
	if weakCC && !req.Confined {
		warnings = append(warnings, "this recording runs with OPEN egress under the WEAKEST available isolation ("+
			string(run.ConfinementClass)+"/runc, shared host kernel) — the mounted workspace is an exfiltration "+
			"window for the duration; the run is audited")
	}
	warnings = append(warnings, recordMaskingCaveat)
	resp["warnings"] = warnings
	writeJSON(w, http.StatusAccepted, resp)
}

// handlePromoteRecordEgress merges one recorded task's OBSERVED-ALLOWED hosts
// into the workspace's operator-owned ApprovedEgress:
// POST /workspaces/{id}/record/{task}/promote-egress. Promotion follows
// recordmode.Synthesize's own rule — only hosts with at least one egress.allow
// decision; a host that was only denied or pending is never promoted (it would
// widen past what even the open run was permitted). Additive and idempotent;
// rides the existing scoped ApprovedEgress lane + audit, so the subsequent
// confined Verify (which unions ApprovedEgress) is widened automatically.
// Nothing is ever auto-applied: this endpoint IS the operator's click.
//
// Optional {"hosts": [...]} narrows promotion to a SUBSET the operator picked
// (e.g. reviewing an allow-all recording's observed set and approving only
// what they recognize instead of the whole thing wholesale): every requested
// host must be a member of the recording's promotable set or the request is
// rejected outright; omitted keeps promoting the full promotable set.
func (s *Server) handlePromoteRecordEgress(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "workspace")
	if !ok {
		return
	}
	taskKey := chi.URLParam(r, "task")
	var req struct {
		Hosts []string `json:"hosts"`
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
	res, ok := recordResultsMap(ws)[taskKey]
	if !ok || res.Observations == nil {
		writeError(w, http.StatusUnprocessableEntity, "task has no captured recording to promote from")
		return
	}
	if res.Status != recordStatusRecorded {
		writeError(w, http.StatusUnprocessableEntity, "recording is "+res.Status+" — only a captured (recorded) task can be promoted")
		return
	}

	// The control plane itself shows up in every capture (the sandbox's
	// brokered result upload is a real, logged egress.allow) — but that's
	// Wardyn's own plumbing, not a task need. Never offer it for promotion:
	// a direct allowlist entry would let future confined sandboxes reach the
	// API surface beyond the proxy's brokered routes.
	selfHost := controlPlaneHost(s.cfg.ControlPlaneURL)
	// The LLM model-provider host(s) (api.anthropic.com, …) are HARNESS
	// plumbing modelProviderEgress already unions into EVERY session, and the
	// baseline clone hosts (scanEgressDomains) are wired into every scan/verify
	// for free — neither is a workspace-specific need, and a promotion here is
	// a PERMANENT per-workspace ApprovedEgress entry. Skip both, exactly like
	// selfHost above.
	skipHost := map[string]struct{}{}
	for _, h := range modelProviderEgress(s.cfg.DefaultPolicy) {
		skipHost[strings.ToLower(strings.TrimSpace(h))] = struct{}{}
	}
	for _, h := range scanEgressDomains(repoCloneURL(ws.Source)) {
		skipHost[strings.ToLower(strings.TrimSpace(h))] = struct{}{}
	}
	// The GitHub clone hosts are now git-broker-managed (Option C: routed through
	// wardyn-proxy, never in a run's egress allowlist). scanEgressDomains no longer
	// returns them, but they must STILL never be promoted to a permanent
	// ApprovedEgress entry — that would re-open host-level github egress and defeat
	// the broker's repo-scoping. Skip them explicitly.
	for _, h := range gitBrokerManagedHosts {
		skipHost[h] = struct{}{}
	}

	// promotable is every host this recording could ever offer up: observed
	// with at least one ALLOW decision, a valid approve-lane host shape, and
	// not plumbing (selfHost / model-provider / baseline clone).
	promotable := map[string]struct{}{}
	for _, d := range res.Observations.Domains {
		if d.AllowCount <= 0 {
			continue // denied/pending-only: never promote past what the open run got
		}
		host := d.Host // Capture already lowercases + trims
		if selfHost != "" && host == selfHost {
			continue
		}
		if _, skip := skipHost[host]; skip {
			continue
		}
		if !workspacescan.ValidApprovedHost(host) {
			continue // e.g. an IP literal or junk — the approve lane wouldn't take it either
		}
		promotable[host] = struct{}{}
	}

	// wantHosts is what THIS request actually promotes: the full promotable
	// set, unless the operator narrowed it to a validated subset.
	var wantHosts []string
	if req.Hosts != nil {
		for _, h := range req.Hosts {
			h = strings.ToLower(strings.TrimSpace(h))
			if h == "" {
				continue
			}
			if _, ok := promotable[h]; !ok {
				writeError(w, http.StatusUnprocessableEntity, "host "+h+" was not observed+allowed in this recording — cannot promote")
				return
			}
			wantHosts = append(wantHosts, h)
		}
	} else {
		for h := range promotable {
			wantHosts = append(wantHosts, h)
		}
	}

	merged := map[string]struct{}{}
	for _, d := range ws.ApprovedEgress {
		merged[d] = struct{}{}
	}
	var promoted []string
	for _, h := range wantHosts {
		if _, dup := merged[h]; dup {
			continue
		}
		merged[h] = struct{}{}
		promoted = append(promoted, h)
	}
	domains := make([]string, 0, len(merged))
	for d := range merged {
		domains = append(domains, d)
	}
	sort.Strings(domains)
	if len(domains) > maxApprovedEgress {
		writeError(w, http.StatusUnprocessableEntity, "promotion would exceed the approved-egress cap (max 64) — prune the list first")
		return
	}

	// M4: the record-entry CAS runs FIRST — only once it succeeds do we apply
	// the ApprovedEgress widening. A CAS miss (the recording changed
	// concurrently) must leave egress untouched, not widen-then-409. Guarded
	// on `recorded`: if a re-record superseded this capture between the
	// operator's read and the click, the marker (and the response) must not
	// resurrect the stale entry.
	res.EgressPromoted = true
	updated, applied, perr := s.putRecordResult(r.Context(), id, taskKey, res, recordStatusRecorded)
	if perr != nil {
		writeError(w, http.StatusInternalServerError, "persist promotion marker: "+perr.Error())
		return
	}
	if !applied {
		writeError(w, http.StatusConflict, "recording changed concurrently (re-record in progress?) — reload and retry")
		return
	}

	if len(promoted) > 0 {
		wsAfter, serr := s.cfg.Store.SetWorkspaceApprovedEgress(r.Context(), id, domains)
		if serr != nil {
			writeError(w, http.StatusInternalServerError, "set approved egress: "+serr.Error())
			return
		}
		updated.ApprovedEgress = wsAfter.ApprovedEgress
		s.recordAudit(r.Context(), s.auditEvent(&res.RunID, actorTypeFromRequest(r), principalFromRequest(r),
			"workspace.egress.approve", id.String(), "success", mustJSON(map[string]any{
				"domains": promoted, "source": "record:" + taskKey,
			})))
	}
	writeJSON(w, http.StatusOK, updated)
}

// controlPlaneHost extracts the lowercase hostname of the configured control
// plane URL ("" when unparseable/unset).
func controlPlaneHost(rawURL string) string {
	u, err := neturl.Parse(strings.TrimSpace(rawURL))
	if err != nil || u == nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
