// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/federation"
	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

func (s *Server) mountRunnerRegistrationRoutes(r chi.Router) {
	r.Post("/runners/register", s.handleRunnerRegister)
	r.Group(func(r chi.Router) {
		r.Use(s.humanOrAdminAuth)
		r.With(s.requireOperator).Post("/runners/tokens", s.handleRunnerTokenAdmin)
		r.Post("/me/runners/tokens", s.handleRunnerTokenSelf)
		r.Post("/me/runners/{id}/claim", s.handleRunnerClaim)
	})
}

func (s *Server) runnerRegistrationStore(w http.ResponseWriter) (store.RunnerRegistrationStore, bool) {
	if err := federation.CheckOrgURL(s.cfg.RunnerOrgURL); err != nil {
		writeErrorReason(w, http.StatusServiceUnavailable, reasonRunnerRegistrationUnavailable, "configure WARDYN_RUNNER_ORG_URL with this organisation's public HTTPS URL to enable runner registration")
		return nil, false
	}
	rs, ok := s.cfg.Store.(store.RunnerRegistrationStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner registration is unavailable")
	}
	return rs, ok
}

// runnerSessionOwner is the signed-in person a runner door acts for: never an administrative or
// delegated credential, which owns no runner. The refusal's reason and sentence are the caller's.
func (s *Server) runnerSessionOwner(w http.ResponseWriter, r *http.Request, reason, msg string) (string, bool) {
	owner := oidcHumanFromContext(r.Context())
	if owner == "" || neverOperator(r.Context()) {
		writeErrorReason(w, http.StatusForbidden, reason, msg)
		return "", false
	}
	return owner, true
}

func (s *Server) runnerPersonalOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	return s.runnerSessionOwner(w, r, string(placement.ReasonRunnerClaimMismatch), "sign in as the runner's owner; administrative and delegated credentials cannot claim for a person")
}

func (s *Server) handleRunnerTokenSelf(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	var req struct{}
	if !decodeStrict(w, r, &req) {
		return
	}
	s.mintRunnerToken(w, r, owner, time.Hour)
}

func (s *Server) handleRunnerTokenAdmin(w http.ResponseWriter, r *http.Request) {
	if !s.requireRunnersEnabled(w, r) {
		return
	}
	var req types.RunnerTokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	owner := strings.TrimSpace(req.Owner)
	if owner == "" || len(owner) > 512 || !controlCharFree(owner) || s.isReservedPrincipal(owner) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRunnerOwnerInvalid, "owner must name a person's principal")
		return
	}
	s.mintRunnerToken(w, r, owner, 72*time.Hour)
}

func (s *Server) mintRunnerToken(w http.ResponseWriter, r *http.Request, owner string, ttl time.Duration) {
	rs, ok := s.runnerRegistrationStore(w)
	if !ok {
		return
	}
	now := s.cfg.Now().UTC()
	raw := newBearer("wdr_")
	token, err := rs.MintRunnerRegistrationToken(r.Context(), raw, types.RunnerRegistrationToken{
		ID: uuid.New(), Owner: owner, MintedBy: principalFromRequest(r), CreatedAt: now,
		ExpiresAt: now.Add(ttl), OrgURLSHA256: federation.OrgURLSHA256(s.cfg.RunnerOrgURL),
	})
	if err != nil {
		writeServerError(w, r, "mint runner registration token", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		placement.ActionRunnerTokenCreate, token.ID.String(), "success", mustJSON(map[string]any{"owner": owner, "expires_at": token.ExpiresAt})))
	token.Token = raw
	writeJSON(w, http.StatusCreated, token)
}
