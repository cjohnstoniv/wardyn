// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// handleMe reports the authenticated principal, how they authenticated, their
// B1-derived role, their session email, and whether they hold the admin role,
// so the UI can show the real signed-in user instead of a placeholder AND hide
// the admin-only actions instead of letting a member discover them as raw
// 403s. It sits behind humanOrAdminAuth, so reaching it already proves
// authentication.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	method := "token"
	switch {
	case s.cfg.LocalMode:
		method = "local"
	case oidc.PrincipalFromContext(r.Context()) != "":
		method = "sso"
	}
	role := oidc.RoleAdmin
	if !s.isOperator(r.Context()) {
		role = oidc.RoleMember
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"principal": principalFromRequest(r),
		"method":    method,
		"role":      role,
		"email":     oidcEmailFromContext(r.Context()),
		// The SAME predicate operatorOnly gates every admin route with
		// (isOperator, http.go) — never a second, driftable copy of the rule.
		// Kept for the console, which already consumes it: operator == role==admin.
		"operator": s.isOperator(r.Context()),
	})
}
