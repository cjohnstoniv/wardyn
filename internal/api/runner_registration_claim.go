// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) handleRunnerClaim(w http.ResponseWriter, r *http.Request) {
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
	if errors.Is(err, store.ErrNotFound) || (err == nil && (row.Owner != owner || row.OrgURLSHA256 != federation.OrgURLSHA256(s.cfg.RunnerOrgURL))) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), "runner claim does not match the owner or fingerprint")
		return
	}
	if err != nil {
		writeServerError(w, r, "read runner claim", err)
		return
	}
	claimed, err := rs.ClaimRunner(r.Context(), id, owner, req.Fingerprint, s.cfg.Now().UTC())
	if errors.Is(err, store.ErrConflict) || errors.Is(err, store.ErrRunnerClaimMismatch) || errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusForbidden, string(placement.ReasonRunnerClaimMismatch), "runner is not claimable by this owner with this fingerprint")
		return
	}
	if err != nil {
		writeServerError(w, r, "claim runner", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), owner, placement.ActionRunnerClaim,
		claimed.ID.String(), "success", mustJSON(map[string]any{"fingerprint": claimed.KeyFingerprint})))
	writeJSON(w, http.StatusOK, types.RunnerRegistration{RunnerID: claimed.ID, Fingerprint: claimed.KeyFingerprint, State: claimed.State, OrgURLSHA256: claimed.OrgURLSHA256})
}
