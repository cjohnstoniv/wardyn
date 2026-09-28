// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"
)

// isReservedPrincipal reports whether p names an identity that is not a
// person: the admin token, the local-mode operator (the configured seat, or any
// "local:" name — the default seat is "local:<os-user>", and a run it created
// outlives a later switch to SSO), a device, or a registered portal. Authorization compares the
// caller's principal to these strings, so a human whose identity-provider
// subject — or whose wdn_ token's replayed principal — equals one would be
// treated as that identity: owning its runs, skipping its re-checks (#1162).
// Every door a human principal enters by refuses one: the SSO callback, a
// session cookie, a wdn_ token, and a portal's token exchange.
//
// Trimmed and case-folded (Unicode: a Kelvin sign in place of the k in
// "admin-token" folds too). No comparison matches a variant today; refusing
// one keeps that true if a comparison ever folds.
func (s *Server) isReservedPrincipal(p string) bool {
	p = strings.ToLower(strings.TrimSpace(p))
	op := strings.ToLower(strings.TrimSpace(s.cfg.LocalOperator))
	return p == adminTokenPrincipal || (op != "" && p == op) ||
		strings.HasPrefix(p, "local:") || strings.HasPrefix(p, "device:") || strings.HasPrefix(p, delegateActorPrefix)
}

// refuseReservedPrincipal answers 401 with msg, and records auth.fail
// reserved_principal, when a request's authenticated principal p is reserved
// — a session cookie or wdn_ token issued before the sign-in callback refused
// one. Reports whether it answered.
func (s *Server) refuseReservedPrincipal(w http.ResponseWriter, r *http.Request, p, msg string) bool {
	if !s.isReservedPrincipal(p) {
		return false
	}
	s.auditAuthFailed(r, authFailedReservedPrincipal)
	writeError(w, http.StatusUnauthorized, msg)
	return true
}
