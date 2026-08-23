// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
)

// revokeSessionsRequest is POST /api/v1/sessions/revoke's body: exactly one
// of Sub (revoke a single principal's sessions) or All (revoke every
// principal's sessions) must be set — see handleRevokeSessions.
type revokeSessionsRequest struct {
	Sub string `json:"sub"`
	All bool   `json:"all"`
}

// handleRevokeSessions is D16's admin surface for "revoke a human now" — the
// OIDC session cookie is stateless (see internal/auth/oidc's package doc), so
// there is no session row to delete; instead this stamps a CUTOFF
// (Config.SessionRevocations) that oidc.Authenticator.Middleware checks on
// every authenticated request, so a still-unexpired session for the target
// stops working on its VERY NEXT request rather than lingering until its own
// Expiry (up to the OIDC-configured token lifetime) — the same behavior a
// logout, an IdP role demotion, or an IdP account disablement could not
// otherwise force before this existed.
//
// Mounted only when OIDC + SessionRevocations are both wired (see routes.go)
// — with no OIDC session mechanism there is nothing to revoke.
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	var body revokeSessionsRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	body.Sub = strings.TrimSpace(body.Sub)

	switch {
	case body.All && body.Sub != "":
		writeError(w, http.StatusBadRequest, `body must set exactly one of "sub" or "all", not both`)
		return
	case body.All:
		if err := s.cfg.SessionRevocations.RevokeAll(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke all sessions: "+err.Error())
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"session.revoke", "*", "success", mustJSON(map[string]any{"scope": "all"})))
	case body.Sub != "":
		if err := s.cfg.SessionRevocations.RevokeSub(r.Context(), body.Sub); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke sessions: "+err.Error())
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"session.revoke", body.Sub, "success", mustJSON(map[string]any{"scope": "sub", "sub": body.Sub})))
	default:
		writeError(w, http.StatusBadRequest, `body must be {"sub":"<principal>"} or {"all":true}`)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
