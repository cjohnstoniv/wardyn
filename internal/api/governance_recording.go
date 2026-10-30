// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The Wardyn boundary's recording minimum (#2967): GET/PUT /governance/recording read and write the
// organisation's recording block (org.Recording in the site-config document) and nothing else, so the
// security tier, which cannot reach /site-config, sets it. One field, no second copy on the boundary.
// A write is a covered governance write, held for a second human under WARDYN_GOVERNANCE_SECOND_HUMAN
// and never exempt: unlocking or lowering the level widens every run.

const (
	govKindRecording     = types.GovernanceTargetRecording
	govRecordingKey      = "recording"
	recordingWriteAction = "governance.recording.write"
)

// recordingState is the block as GET returns it, a PUT replaces it and an approval compares it: an
// unset block reads as its zero value (the terminal default, unlocked).
func recordingState(sc types.SiteConfig) types.SiteRecording {
	if sc.Recording == nil {
		return types.SiteRecording{}
	}
	return *sc.Recording
}

// withRecording stores b, an all-default block as none, as PUT /site-config does.
func withRecording(sc types.SiteConfig, b types.SiteRecording) types.SiteConfig {
	sc.Recording = nil
	if b != (types.SiteRecording{}) {
		sc.Recording = &b
	}
	return sc
}

// recordingAuditData is the row's level and lock after and before, as the boundary's mode and was_mode.
func recordingAuditData(before, after types.SiteRecording) map[string]any {
	return map[string]any{"level": string(after.OrgLevel()), "locked": after.Locked,
		"was_level": string(before.OrgLevel()), "was_locked": before.Locked}
}

// recordingRefusal refuses a level that is not one, and a locked minimum above off while this
// organisation keeps no session recordings (PUT /site-config's own two refusals).
func (s *Server) recordingRefusal(b types.SiteRecording) *unitWriteError {
	if err := validateSiteRecording(&b); err != nil {
		return writeRefusal(http.StatusBadRequest, reasonRecordingInvalid, "invalid recording: %s", err.Error())
	}
	if b.Locked && b.OrgLevel() != types.RecordingOff && s.cfg.RecordingStore == nil {
		return writeRefusal(placement.ReasonRecordingStoreOff.Status(), string(placement.ReasonRecordingStoreOff), "%s", runnerpool.RecordingSaveStoreOffMsg())
	}
	return nil
}

const recordingStaleMsg = "If-Match does not match the current recording level — GET /governance/recording again and retry"

// handleGetRecording returns the organisation's recording block. securityOps (routes.go).
func (s *Server) handleGetRecording(w http.ResponseWriter, r *http.Request) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	state := recordingState(sc)
	w.Header().Set("ETag", computeETag(state))
	writeJSON(w, http.StatusOK, state)
}

// handlePutRecording replaces the recording block; a body naming any other field is a 400 (decodeStrict).
// securityOps (routes.go). If-Match is optional optimistic concurrency against GET's ETag.
func (s *Server) handlePutRecording(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body types.SiteRecording
	if !decodeStrict(w, r, &body) {
		return
	}
	if refusal := s.recordingRefusal(body); refusal != nil {
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
		return
	}
	if mode == govQueue {
		s.holdRecording(w, r, body)
		return
	}
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return
	}
	defer unlock()
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	before := recordingState(sc)
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonRecordingStale, recordingStaleMsg)
		return
	}
	if _, err := s.cfg.Store.PutSiteConfig(r.Context(), withRecording(sc, body)); err != nil {
		writeServerError(w, r, "put site config", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		recordingWriteAction, govRecordingKey, "success", mustJSON(recordingAuditData(before, body))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindRecording, govRecordingKey, "success", nil)
	}
	w.Header().Set("ETag", computeETag(body))
	writeJSON(w, http.StatusOK, body)
}

// holdRecording stores a validated replacement as a pending change. The approval compares the base
// hash, which is the ETag If-Match is judged against here.
func (s *Server) holdRecording(w http.ResponseWriter, r *http.Request, after types.SiteRecording) {
	var before types.SiteRecording
	err := s.readGovernanceState(r, func(q store.Querier) error {
		sc, err := store.GetSiteConfigQ(r.Context(), q)
		before = recordingState(sc)
		return err
	})
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	if !ifMatchSatisfied(r, computeETag(before)) {
		writeErrorReason(w, http.StatusPreconditionFailed, reasonRecordingStale, recordingStaleMsg)
		return
	}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindRecording, op: "replace", key: govRecordingKey, payload: after,
		before: before, after: after, changed: changedPaths(before, after), baseState: before,
	})
}

// applyRecordingChange applies a held replacement inside the decision transaction: the target lock,
// then the site-config lock every site-config writer holds, the block compared with what the proposal
// reviewed, then the whole document written back on q.
func applyRecordingChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var body types.SiteRecording
	if err := decodeHeldPayload(ch.Payload, &body); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if refusal := s.recordingRefusal(body); refusal != nil {
		return govApplied{}, refusal
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindRecording); err != nil {
		return govApplied{}, err
	}
	if err := store.LockSiteConfig(ctx, q); err != nil {
		return govApplied{}, err
	}
	sc, err := store.GetSiteConfigQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	before := recordingState(sc)
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if _, err := store.PutSiteConfigQ(ctx, q, withRecording(sc, body)); err != nil {
		return govApplied{}, err
	}
	return govApplied{action: recordingWriteAction, target: govRecordingKey, data: recordingAuditData(before, body)}, nil
}
