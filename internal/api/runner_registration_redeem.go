// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
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
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	now := s.cfg.Now().UTC()
	if !s.registerLimiter.allow(peerKey(r.RemoteAddr), now) {
		w.Header().Set("Retry-After", "10")
		writeErrorReason(w, http.StatusTooManyRequests, reasonRunnerRegistrationRateLimited, "too many registration attempts; retry later")
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
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerRegistrationInvalid, "registration requires an Ed25519 public key and a name of at most 200 bytes without control characters")
		return
	}
	// Every runner declares its limits at registration (wardyn-runnerd measures the host and proposes
	// them): a register without them, an older wardyn-runnerd's included, is refused before the token
	// is spent, so the person can register again with this release's runner.
	if req.Limits == nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerLimitsRequired,
			"registration requires the runner's limits: runs at once, total and per run. Register with this release's wardyn-runnerd.")
		return
	}
	if err := req.Limits.Validate(types.RunnerMaxRunsCeiling); err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerLimitsRequired, "runner limits: "+err.Error())
		return
	}
	req.MachineName = strings.TrimSpace(req.MachineName)
	if !types.ValidMachineName(req.MachineName) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerRegistrationInvalid, "machine_name must be at most 253 bytes of printable characters")
		return
	}
	if !types.ValidArch(req.Arch) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerRegistrationInvalid, `arch must be "amd64" or "arm64"`)
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
	// A valid token is the caller's proof: only failed attempts are charged to the address, which many
	// callers share behind an ingress.
	s.registerLimiter.refund(peerKey(r.RemoteAddr), now)
	registered, err := rs.CreateRunner(r.Context(), types.Runner{ID: uuid.New(), Owner: token.Owner,
		Name: req.Name, PublicKey: req.PublicKey, KeyFingerprint: runnerwire.Fingerprint(req.PublicKey), OrgURLSHA256: orgHash, MintedBy: token.MintedBy,
		Limits: req.Limits, MachineName: req.MachineName, Arch: req.Arch, OwnerKind: token.OwnerKind, RegistrationTokenID: &token.ID})
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, reasonRunnerKeyAlreadyRegistered, "this key was already registered; generate a new identity and registration token")
		return
	}
	if err != nil {
		writeServerError(w, r, "register runner", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, types.ActorSystem, "runner:"+registered.ID.String(),
		placement.ActionRunnerEnrol, registered.ID.String(), "success", mustJSON(map[string]any{"token_id": token.ID, "fingerprint": registered.KeyFingerprint, "limits": registered.Limits, "owner_kind": registered.OwnerKind})))
	writeJSON(w, http.StatusCreated, types.RunnerRegistration{RunnerID: registered.ID, Fingerprint: registered.KeyFingerprint, State: registered.State, OrgURLSHA256: orgHash, Limits: registered.Limits,
		ProxyImage: cmp.Or(s.cfg.ProxyImageByArch[req.Arch], s.cfg.ProxyImage)})
}
