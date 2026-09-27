// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// GET /approvals' opt-in ?view= (#1197). Split out of approvals.go, which
// sits at the file-size gate's cap.
package api

import "net/http"

// approvalsViewScope resolves ?view=user|admin for handleListApprovals: it
// reports whether THIS request must be scoped to the caller's own runs'
// approvals, the same shape as GET /runs' owner-forcing fix. view=user
// forces it for EVERY caller, admins and security operators included;
// absent, or view=admin from
// a non-operator (coerced to user, fail closed), changes nothing — a
// non-operator was ALREADY always scoped by handleListApprovals' existing
// branch, so this can only ever narrow an operator's read, never widen a
// member's. Writes a 400 and returns ok=false on an unrecognised view;
// callers must return immediately when ok is false.
func (s *Server) approvalsViewScope(w http.ResponseWriter, r *http.Request) (scopeToOwner, ok bool) {
	view := r.URL.Query().Get("view")
	switch view {
	case "", "user", "admin":
	default:
		writeError(w, http.StatusBadRequest, "invalid view")
		return false, false
	}
	isOperator := s.isSecurityOperator(r.Context())
	if view == "admin" && !isOperator {
		view = "user"
	}
	return !isOperator || view == "user", true
}
