// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/internal/workspacescan"
)

// handleUploadVerifyResult accepts a PUT /api/v1/internal/verify-results/{runID}
// from wardyn-verify running inside a governed verify run. The exact sibling of
// handleUploadScanResult: run-token auth, cross-run guard, size cap, strict
// unmarshal, TRUSTED run→workspace linkage from the run row (never sandbox
// input), and re-derive-not-trust — the VerifyResult is re-validated + secret-
// masked control-plane-side before it becomes the workspace's authority.
func (s *Server) handleUploadVerifyResult(w http.ResponseWriter, r *http.Request) {
	// Cross-run guard + TRUSTED run→workspace linkage, bound to run KIND: only a
	// verify or record run may post a verify-shaped result (a scan run that got
	// compromised can't smuggle one, and vice versa). A record run's upload
	// lands in ITS OWN record_results lane — it can never touch verify_result,
	// status, or the verified markers.
	claims, run, ok := s.authSandboxRunUpload(w, r,
		"run not found for verify upload", "run is not a governed workspace run", "run is not a verify run",
		"workspace verify", "workspace record")
	if !ok {
		return
	}
	isRecord := run.Task == "workspace record"
	wsID := *run.WorkspaceID

	limited := http.MaxBytesReader(w, r.Body, maxScanResultUploadBytes)
	raw, err := io.ReadAll(limited)
	if err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, "verify result exceeds size limit")
			return
		}
		writeError(w, http.StatusBadRequest, "read verify result: "+err.Error())
		return
	}
	var uploaded workspacescan.VerifyResult
	if jerr := json.Unmarshal(raw, &uploaded); jerr != nil {
		writeError(w, http.StatusBadRequest, "invalid verify result: "+jerr.Error())
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

	// Re-derive: cap/validate steps, mask secret-shaped tokens out of logs,
	// recompute OK from exit codes (never trust the uploader's flag).
	result := workspacescan.DeriveVerifyResult(uploaded)

	// RECORD run: stream the per-step outcome into the task's record_results
	// entry (live UI) and stop — the task stays `recording` until the run
	// terminates and reconcileRecordRun captures the audit evidence. The
	// workspace's verify state is untouchable from this lane by construction.
	if isRecord {
		taskKey := ""
		var res RecordTaskResult
		for k, v := range recordResultsMap(ws) {
			if v.RunID == claims.RunID {
				taskKey, res = k, v
				break
			}
		}
		if taskKey == "" || res.Status != recordStatusRecording {
			writeError(w, http.StatusConflict, "no in-flight recording for this run")
			return
		}
		res.Steps = result.Steps
		if result.FailureHint != "" {
			res.FailureHint = result.FailureHint
		}
		// Compare-and-set on `recording`: a late upload that races the terminal
		// capture can never revert a completed entry — it just no-ops.
		if _, _, perr := s.putRecordResult(r.Context(), wsID, taskKey, res, recordStatusRecording); perr != nil {
			writeError(w, http.StatusInternalServerError, "persist record steps: "+perr.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// VERIFY fence (H6): only the workspace's CURRENT active run may write verify
	// results. A late Done=true upload from a killed/reaped/superseded verify run
	// must NOT finalize the workspace (flip verify_failed->ready, stamp
	// VerifiedProfileHash/VerifiedAt) — nor may a superseded run's PROGRESS upload
	// re-claim active_run_id. This mirrors the CAS fence the record lane above
	// enforces via its recording-status match. launchVerifyRun claims active_run_id
	// before dispatch (M1), so a legitimate in-flight run always matches.
	if ws.ActiveRunID == nil || *ws.ActiveRunID != claims.RunID {
		writeError(w, http.StatusConflict, "this run is not the workspace's active verify run (superseded, killed, or already finalized)")
		return
	}

	// PROGRESS upload (Done=false): persist the partial result for the live UI but
	// stay `verifying` and keep the in-flight run pointer — do NOT finalize.
	if !result.Done {
		if _, err := s.cfg.Store.SetWorkspaceImportState(r.Context(), wsID, types.WorkspaceVerifying,
			&claims.RunID, mustJSON(result), ws.VerifiedProfileHash, ws.VerifiedAt); err != nil {
			writeError(w, http.StatusInternalServerError, "persist verify progress: "+err.Error())
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}

	// FINAL upload: finalize the workspace status.
	status := types.WorkspaceVerifyFailed
	// Preserve any prior green markers on a FAILED re-verify (the image the last
	// success proved is unchanged) — only a green verify updates them.
	verifiedHash := ws.VerifiedProfileHash
	verifiedAt := ws.VerifiedAt
	if result.OK {
		status = types.WorkspaceReady
		verifiedHash = ws.BuiltProfileHash // the image that was proven to work
		now := s.cfg.Now().UTC()
		verifiedAt = &now
	}
	// Clear active_run_id (the verify run is done) and persist the result.
	if _, err := s.cfg.Store.SetWorkspaceImportState(r.Context(), wsID, status, nil,
		mustJSON(result), verifiedHash, verifiedAt); err != nil {
		writeError(w, http.StatusInternalServerError, "persist verify result: "+err.Error())
		return
	}

	// Content-free audit: step count + outcome + failing stage only, never logs.
	failedStage := ""
	for _, st := range result.Steps {
		if st.ExitCode != 0 || st.TimedOut {
			failedStage = st.Stage
			break
		}
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"workspace.verify", wsID.String(), outcomeBool(result.OK), mustJSON(map[string]any{
			"steps": len(result.Steps), "ok": result.OK, "ran": result.Ran, "failed_stage": failedStage,
		})))
	w.WriteHeader(http.StatusNoContent)
}

func outcomeBool(ok bool) string {
	if ok {
		return "success"
	}
	return "failure"
}
