// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// handleMe reports the authenticated principal, how they authenticated, and
// whether they hold the operator role, so the UI can show the real signed-in
// user instead of a placeholder AND hide the operator-only actions instead of
// letting a viewer discover them as raw 403s. It sits behind humanOrAdminAuth,
// so reaching it already proves authentication.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	method := "token"
	switch {
	case s.cfg.LocalMode:
		method = "local"
	case oidc.PrincipalFromContext(r.Context()) != "":
		method = "sso"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"principal": principalFromRequest(r),
		"method":    method,
		// The SAME predicate operatorOnly gates the 24 operator routes with
		// (isOperator, http.go) — never a second, driftable copy of the rule.
		"operator": s.isOperator(r.Context()),
	})
}
