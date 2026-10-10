// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ed25519"
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) handleRunnerRegister(w http.ResponseWriter, r *http.Request) {
	now := s.cfg.Now().UTC()
	if !s.enrolLimiter.allow(peerKey(r.RemoteAddr), now) {
		w.Header().Set("Retry-After", "10")
		writeErrorReason(w, http.StatusTooManyRequests, "runner_registration_rate_limited", "too many registration attempts; retry later")
		return
	}
	rs, ok := s.runnerRegistrationStore(w)
	if !ok {
		return
	}
	var req types.RunnerRegisterRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if len(req.PublicKey) != ed25519.PublicKeySize || req.Name == "" || len(req.Name) > 200 || !controlCharFree(req.Name) {
		writeErrorReason(w, http.StatusUnprocessableEntity, "runner_registration_invalid", "registration requires an Ed25519 public key and a name of at most 200 bytes without control characters")
		return
	}
	orgHash := federation.OrgURLSHA256(s.cfg.RunnerOrgURL)
	token, valid, err := rs.ConsumeRunnerRegistrationToken(r.Context(), req.Token, orgHash, s.cfg.Now().UTC())
	if err != nil {
		writeServerError(w, r, "consume runner registration token", err)
		return
	}
	if !valid {
		s.auditAuthFailedAs(r, "wardyn/runnerRegister", "invalid_runner_token")
		writeErrorReason(w, http.StatusUnauthorized, string(placement.ReasonRunnerTokenInvalid), "registration token is invalid, expired or already used")
		return
	}
	registered, err := rs.CreateRunner(r.Context(), types.Runner{ID: uuid.New(), Owner: token.Owner,
		Name: req.Name, PublicKey: req.PublicKey, KeyFingerprint: runnerwire.Fingerprint(req.PublicKey), OrgURLSHA256: orgHash})
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, "runner_key_already_registered", "this key was already registered; generate a new identity and registration token")
		return
	}
	if err != nil {
		writeServerError(w, r, "register runner", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, types.ActorSystem, "runner:"+registered.ID.String(),
		placement.ActionRunnerEnrol, registered.ID.String(), "success", mustJSON(map[string]any{"token_id": token.ID, "fingerprint": registered.KeyFingerprint})))
	writeJSON(w, http.StatusCreated, types.RunnerRegistration{RunnerID: registered.ID, Fingerprint: registered.KeyFingerprint, State: registered.State, OrgURLSHA256: orgHash})
}
