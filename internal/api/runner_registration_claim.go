// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) handleRunnerClaim(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	rs, ok := s.runnerRegistrationStore(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	var req types.RunnerClaimRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	row, err := rs.GetRunner(r.Context(), id)
	// Another person's runner and an absent one are the same answer, so a refusal never says whose runner it is.
	if errors.Is(err, store.ErrNotFound) || (err == nil && row.Owner != owner) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), "runner claim does not match the owner or fingerprint")
		return
	}
	if err != nil {
		writeServerError(w, r, "read runner claim", err)
		return
	}
	now := s.cfg.Now().UTC()
	if msg := runnerClaimRefusal(row, federation.OrgURLSHA256(s.cfg.RunnerOrgURL), now); msg != "" {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), msg)
		return
	}
	claimed, err := rs.ClaimRunner(r.Context(), id, owner, req.Fingerprint, now)
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), runnerClaimAlreadyMsg)
		return
	}
	if errors.Is(err, store.ErrRunnerClaimMismatch) || errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), "the fingerprint does not match the one this runner registered with; compare it with what the runner printed")
		return
	}
	if err != nil {
		writeServerError(w, r, "claim runner", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), owner, placement.ActionRunnerClaim,
		claimed.ID.String(), "success", mustJSON(map[string]any{"fingerprint": claimed.KeyFingerprint})))
	writeJSON(w, http.StatusOK, types.RunnerRegistration{RunnerID: claimed.ID, Fingerprint: claimed.KeyFingerprint, State: claimed.State, OrgURLSHA256: claimed.OrgURLSHA256, Limits: claimed.Limits})
}

const runnerClaimAlreadyMsg = "this runner has already been claimed"

// runnerClaimRefusal says why the owner's own runner row cannot be claimed, or "" when it can. It runs only
// after the owner matched, so naming the cause tells the owner nothing about anyone else's runner.
func runnerClaimRefusal(row types.Runner, orgHash string, now time.Time) string {
	switch {
	case row.OrgURLSHA256 != orgHash:
		return "this runner registered against a different organisation URL than this control plane's now; register it again"
	case row.State == types.RunnerRevoked:
		return "this runner was revoked and cannot be claimed; register it again"
	case row.State != types.RunnerUnclaimed:
		return runnerClaimAlreadyMsg
	case row.ClaimExpired(now):
		return "this runner's registration expired before it was claimed; mint a new token and register it again"
	}
	return ""
}
