// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// maxScanResultUploadBytes caps a single scan-result PUT. ScanFacts is already
// bounded on the producing side (internal/workspacescan manifest-count + per-file
// caps), so this is a generous DoS ceiling on a hostile in-sandbox agent that
// ignores those bounds and streams junk. Bytes beyond the cap error the reader,
// surfaced as 413.
const maxScanResultUploadBytes = 8 << 20 // 8 MiB

// handleUploadScanResult accepts a PUT /api/v1/internal/scan-results/{runID}
// from wardyn-scan running inside a governed scan run. The caller must hold a
// valid run token (internalAuth); the path run id MUST match the token's run
// (cross-run pollution guard, exactly like handleUploadRecording). The body is
// the raw ScanFacts JSON — untrusted evidence, re-derived into a WorkspaceProfile
// control-plane-side (facts-out, not profile-out).
//
// Run→workspace linkage: which workspace a run scans is established by the
// scan-run launcher, which is a LATER wave (A4/Wave-3). For now the target
// workspace is taken from a ?workspace_id= query param.
//
// TODO(Wave-3 launcher): replace the query param with a TRUSTED server-side
// run→workspace lookup (e.g. keyed on claims.RunID). The proxy's brokered route
// deliberately does NOT forward the sandbox's query string, so the sandbox
// cannot target an arbitrary workspace today; the launcher must supply the id
// from trusted state, NOT from sandbox input.
func (s *Server) handleUploadScanResult(w http.ResponseWriter, r *http.Request) {
	// Cross-run guard + TRUSTED run→workspace linkage: the caller must hold the
	// scan run's OWN token, and the run must be a governed scan run (nil
	// WorkspaceID or a non-scan Task has no business uploading scan facts).
	claims, scanRun, ok := s.authSandboxRunUpload(w, r,
		"run not found for scan upload", "run is not a governed scan run", "run is not a scan run",
		"workspace scan")
	if !ok {
		return
	}
	wsID := *scanRun.WorkspaceID

	// Fail closed: cap the body, then strict-parse it. A malformed / oversized
	// body never yields a profile (it would otherwise let an in-sandbox agent
	// pollute a workspace's authority object).
	raw, ok := readCappedBody(w, r, maxScanResultUploadBytes, "scan result")
	if !ok {
		return
	}
	// Fail-closed parse: a malformed body is rejected outright — never partially
	// applied. Trust does NOT come from strict field matching (DeriveProfile is
	// explicitly untrusted-input-safe: it ignores unknown marker ids and only
	// ever maps facts onto the fixed markers.go egress table), so a plain
	// Unmarshal is both sufficient and forward-compatible with a newer scanner.
	var facts workspacescan.ScanFacts
	if err := json.Unmarshal(raw, &facts); err != nil {
		s.auditScan(r, claims.SPIFFEID, wsID, "failure", "parse: "+err.Error(), nil)
		writeError(w, http.StatusBadRequest, "invalid scan facts: "+err.Error())
		return
	}

	ws, err := s.cfg.Store.GetWorkspace(r.Context(), wsID)
	if notFoundIf(w, err, "workspace") {
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get workspace: "+err.Error())
		return
	}

	// Re-derive the authority object from the untrusted facts, persist it, and
	// flip the workspace to ready. Identity fields are preserved from the fetched
	// row; only the scan-owned fields change.
	profile := workspacescan.DeriveProfile(facts)

	// ADVISORY AI fallback (opt-in; nil advisor = OFF, byte-identical behavior).
	// Only when the deterministic pass is unsure (ShouldAdvise). AdviseProfile
	// gap-fills EMPTY fields, can only RAISE NeedsReview, and FAILS OPEN — any
	// advisor error (incl. a bounded CLI timeout) keeps `profile` unchanged, so it
	// can never fail the sidecar's upload. It flips Source to SourceAIAssisted iff
	// it actually changed something (our audit discriminator below).
	aiRan, aiChanged := false, false
	if s.cfg.ScanAIAdvisor != nil && workspacescan.ShouldAdvise(profile, facts) {
		aiRan = true
		profile = s.cfg.ScanAIAdvisor(r.Context(), facts, profile)
		aiChanged = profile.Source == workspacescan.SourceAIAssisted
	}

	ws.Profile = mustJSON(profile)
	// Scanned, not ready: the import flow continues (configure → verify →
	// finalize). `ready` now means the import was finalized/verified.
	ws.Status = types.WorkspaceScanned
	if _, err := s.cfg.Store.UpdateWorkspace(r.Context(), wsID, ws); err != nil {
		s.auditScan(r, claims.SPIFFEID, wsID, "failure", "persist: "+err.Error(), nil)
		writeError(w, http.StatusInternalServerError, "persist scan profile: "+err.Error())
		return
	}

	// Counts only — never detected names (and never values) in audit data.
	s.auditScan(r, claims.SPIFFEID, wsID, "success", "", map[string]any{
		"confidence": profile.Confidence, "needs_review": profile.NeedsReview,
		"secret_reqs": len(profile.RequiredSecrets), "services": len(profile.ServicesNeeded),
		"suggested_egress": len(profile.SuggestedEgress), "secret_files": len(profile.SecretFilesPresent),
		"leak_findings": len(profile.LeakFindings), "build_mem_mib": profile.BuildMemoryMiB,
		// AI-advisor discriminator: ai_advisor=whether the advisory fallback ran,
		// ai_changed=whether it altered the deterministic profile (source flip).
		"ai_advisor": aiRan, "ai_changed": aiChanged,
	})
	w.WriteHeader(http.StatusNoContent)
}

// auditScan records a workspace.scan audit event attributed to the scanning
// agent run (mirrors handleUploadRecording's recording.upload audit). extra
// carries scan-summary COUNTS (never detected names or values).
func (s *Server) auditScan(r *http.Request, spiffeID string, wsID uuid.UUID, outcome, detail string, extra map[string]any) {
	claims, _ := claimsFromContext(r)
	var runID *uuid.UUID
	data := map[string]any{"workspace_id": wsID.String()}
	if claims != nil {
		rid := claims.RunID
		runID = &rid
		data["run_id"] = rid.String()
	}
	if detail != "" {
		data["detail"] = detail
	}
	for k, v := range extra {
		data[k] = v
	}
	s.recordAudit(r.Context(), s.auditEvent(
		runID, types.ActorAgent, spiffeID, "workspace.scan", wsID.String(), outcome, mustJSON(data),
	))
}
