// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity"
	"github.com/cjohnstoniv/wardyn/internal/store"
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
// from wardyn-scan running inside a governed SOURCE scan run. The caller must
// hold a valid run token (internalAuth); the path run id MUST match the
// token's run (cross-run pollution guard, exactly like handleUploadRecording).
// Every governed scan run carries a SourceID (the three-tier retarget's only
// producer, launchSourceScanRun, always pairs Task "source scan" with one) —
// there is no separate workspace lane: a workspace's profile is always the
// hydrate pass's merge of its attached sources' own scan results.
func (s *Server) handleUploadScanResult(w http.ResponseWriter, r *http.Request) {
	// Cross-run guard + TRUSTED run→workspace linkage: the caller must hold the
	// scan run's OWN token, and the run must be a governed source scan run (nil
	// SourceID or a non-scan Task has no business uploading scan facts).
	claims, scanRun, ok := s.authSandboxRunUpload(w, r,
		"run not found for scan upload", "run is not a governed scan run", "run is not a scan run",
		"source scan")
	if !ok {
		return
	}
	if scanRun.SourceID == nil {
		writeError(w, http.StatusForbidden, "run is not a governed scan run")
		return
	}
	s.uploadSourceScanResult(w, r, claims, *scanRun.SourceID)
}

// uploadSourceScanResult is the (only live) lane of handleUploadScanResult:
// capped read, fail-closed parse, facts→profile derivation plus the opt-in
// AI-advisor gap-fill, landing on the source row via the fenced
// SetSourceScanResult, so a superseded upload (the fence moved) fails fast
// and honestly rather than clobbering.
func (s *Server) uploadSourceScanResult(w http.ResponseWriter, r *http.Request, claims *identity.Claims, sourceID uuid.UUID) {
	// Fail closed: cap the body, then strict-parse it. A malformed / oversized
	// body never yields a profile (it would otherwise let an in-sandbox agent
	// pollute a source's authority object).
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
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"source.scan", sourceID.String(), "failure", mustJSON(map[string]any{"detail": "parse: " + err.Error()})))
		writeError(w, http.StatusBadRequest, "invalid scan facts: "+err.Error())
		return
	}
	profile := workspacescan.DeriveProfile(facts)
	profile, aiRan, aiChanged := s.applyScanAIAdvisor(r.Context(), facts, profile)

	// The scan's discoveries land on the SOURCE's own contract in the same
	// fenced write — a repo source has no host write path, so its seed is
	// profile-derived only (secrets + auto-allowed hosts).
	src, err := s.cfg.Store.SetSourceScanResult(r.Context(), sourceID, mustJSON(profile), types.WorkspaceScanned, claims.RunID, seedSourceRequirements(types.SourceRepo, "", profile))
	if errors.Is(err, store.ErrNotFound) {
		s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
			"source.scan", sourceID.String(), "failure", mustJSON(map[string]any{"detail": "superseded scan upload (fence mismatch)"})))
		writeError(w, http.StatusConflict, "scan upload superseded: another scan owns this source")
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, "persist source profile: "+err.Error())
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(&claims.RunID, types.ActorAgent, claims.SPIFFEID,
		"source.scan", sourceID.String(), "success", mustJSON(map[string]any{
			"confidence": profile.Confidence, "secret_reqs": len(profile.RequiredSecrets),
			"suggested_egress": len(profile.SuggestedEgress), "leak_findings": len(profile.LeakFindings),
			// AI-advisor discriminator: ai_advisor=whether the advisory fallback ran,
			// ai_changed=whether it altered the deterministic profile (source flip).
			"ai_advisor": aiRan, "ai_changed": aiChanged,
		})))
	writeJSON(w, http.StatusOK, map[string]any{"source_id": src.ID, "status": src.Status})
}

// applyScanAIAdvisor runs the opt-in ADVISORY AI gap-fill (nil advisor = OFF,
// byte-identical behavior) on facts/profile, only when the deterministic pass
// is unsure (ShouldAdvise). AdviseProfile gap-fills EMPTY fields, can only
// RAISE NeedsReview, and FAILS OPEN — any advisor error (incl. a bounded CLI
// timeout) keeps profile unchanged, so a caller's own persist/upload can never
// fail because of it. aiChanged reports whether it actually flipped Source to
// SourceAIAssisted (the audit discriminator every caller records).
//
// Shared by every scan LANE (W9-S1-6): the sandboxed repo-scan upload
// (uploadSourceScanResult) and the host-side local_dir scan
// (scanLocalDirSource) both derive a profile from ScanFacts, so both get the
// SAME advisory gap-fill on the SAME gate — a local_dir source is not a
// second-class scan lane the flag quietly skips.
func (s *Server) applyScanAIAdvisor(ctx context.Context, facts workspacescan.ScanFacts, profile workspacescan.WorkspaceProfile) (result workspacescan.WorkspaceProfile, aiRan, aiChanged bool) {
	if s.cfg.ScanAIAdvisor == nil || !workspacescan.ShouldAdvise(profile, facts) {
		return profile, false, false
	}
	advised := s.cfg.ScanAIAdvisor(ctx, facts, profile)
	return advised, true, advised.Source == workspacescan.SourceAIAssisted
}
