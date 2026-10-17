// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// isReservedPrincipal reports whether p names an identity that is not a
// person: a service identity ("service:<name>", which authenticates only by its own tokens), the admin token, the local-mode operator (the configured seat, or any
// "local:" name — the default seat is "local:<os-user>", and a run it created
// outlives a later switch to SSO), a device, a registered portal, or a person's
// audit subject ("subject:<id>", the actor WARDYN_AUDIT_SEAL=full stores, refused
// whether or not it is on). Authorization compares the
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
	if strings.HasPrefix(p, types.ServicePrincipalPrefix) { // a service identity (#1508): authenticates only by its own tokens
		return true
	}
	op := strings.ToLower(strings.TrimSpace(s.cfg.LocalOperator))
	return p == adminTokenPrincipal || (op != "" && p == op) ||
		strings.HasPrefix(p, "local:") || strings.HasPrefix(p, "device:") || strings.HasPrefix(p, delegateActorPrefix) ||
		strings.HasPrefix(p, audit.SubjectActorPrefix)
}

// subjectNameReservedMsg is the 422 reserved_principal sentence, after the field's name, for a
// person's name given as a person's audit subject.
const subjectNameReservedMsg = "that name is reserved for a person's audit subject, not a person"

// isSubjectName reports whether name is a person's audit subject ("subject:<id>"). Every route that
// resolves a person's name by directory lookup refuses one: it is not a name, and must never reach one.
func isSubjectName(name string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(name)), audit.SubjectActorPrefix)
}

// refuseSubjectPrincipalParam answers 422 reserved_principal, before the
// handler runs, for a /people/{principal} path naming a person's audit subject.
func (s *Server) refuseSubjectPrincipalParam(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isSubjectName(principalParam(r)) {
			writeErrorReason(w, http.StatusUnprocessableEntity, reasonReservedPrincipal, "principal: "+subjectNameReservedMsg)
			return
		}
		next.ServeHTTP(w, r)
	})
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
	// reasonReservedPrincipal is the SAME value as authFailedReservedPrincipal
	// (oidc.DenialReservedPrincipal) — a reasons_routes.go literal so the docs
	// guard (which reads both reasons files) can see it, #656 slice 3.
	writeErrorReason(w, http.StatusUnauthorized, reasonReservedPrincipal, msg)
	return true
}
