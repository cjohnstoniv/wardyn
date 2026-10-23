// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"

	"github.com/go-chi/chi/v5"
)

// secrets_explicit.go is #1510: whose a stored secret is stated by the route, never inferred from
// the caller's role. PUT/DELETE /secrets/{name} stays as it was (a super admin's write lands in the
// shared operator namespace, everyone else's in their own, which a personal API token of an admin
// cannot change); these routes say which they mean.
//
//	/me/secrets/{name}      the caller's own namespace, for any role.
//	/shared-secrets/{name}  the shared operator namespace, operator tier only.
//
// Neither takes ?owner=, and neither can write into another person's namespace.

// The secret.write and secret.delete audit rows of the explicit routes carry scope, so an
// investigator can tell a deliberate personal or shared write from one the legacy route placed by
// role. The legacy route's rows carry none and are unchanged.
const (
	secretScopePersonal = "personal"
	secretScopeShared   = "shared"
)

const (
	secretPersonalNeedsPersonRefusal = "your own secrets belong to a signed-in person; this caller (the admin token, local mode or automation) has no personal namespace. " +
		"Use PUT /shared-secrets/{name} for a shared service secret"
	secretExplicitOwnerRefusal = "?owner= is not accepted here: this route names its namespace itself, and nobody sets or removes a secret for another person on it"
)

func (s *Server) mountExplicitSecretRoutes(r, operatorOnly chi.Router) {
	r.Get("/me/secrets", s.handleListMySecrets)
	r.Put("/me/secrets/{name}", s.handlePutMySecret)
	r.Delete("/me/secrets/{name}", s.handleDeleteMySecret)
	operatorOnly.Get("/shared-secrets", s.handleListSharedSecrets)
	operatorOnly.Put("/shared-secrets/{name}", s.handlePutSharedSecret)
	operatorOnly.Delete("/shared-secrets/{name}", s.handleDeleteSharedSecret)
}

// personalSecretOwner is the caller's own namespace: the verified person's subject, whatever their
// role and whichever view the console shows. A caller with no verified person (the admin token,
// local mode, a device) has none and is refused, because a namespace derived from an attribution
// string could be one nothing reads. A portal acting for someone is refused first, as delegation_scope.
func (s *Server) personalSecretOwner(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.refuseDelegated(w, r) {
		return "", false
	}
	if neverOperator(r.Context()) || oidcHumanFromContext(r.Context()) == "" {
		writeErrorReason(w, http.StatusForbidden, reasonSecretPersonalNeedsPerson, secretPersonalNeedsPersonRefusal)
		return "", false
	}
	return oidcHumanFromContext(r.Context()), true
}

// refuseExplicitOwnerParam answers ?owner= on an explicit route, audited like the legacy route's.
func (s *Server) refuseExplicitOwnerParam(w http.ResponseWriter, r *http.Request, action, name string) bool {
	if !r.URL.Query().Has("owner") {
		return false
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		action, name, "denied", mustJSON(map[string]any{"reason": "owner_param"})))
	writeErrorReason(w, http.StatusForbidden, reasonSecretOwnerParamRefused, secretExplicitOwnerRefusal)
	return true
}

func (s *Server) handlePutMySecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if s.refuseDelegated(w, r) || s.refuseExplicitOwnerParam(w, r, "secret.write", name) {
		return
	}
	if owner, ok := s.personalSecretOwner(w, r); ok {
		s.putSecretFor(w, r, name, owner, secretScopePersonal)
	}
}

func (s *Server) handleDeleteMySecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if s.refuseDelegated(w, r) || s.refuseExplicitOwnerParam(w, r, "secret.delete", name) {
		return
	}
	if owner, ok := s.personalSecretOwner(w, r); ok {
		s.deleteSecretFrom(w, r, name, owner, true, secretScopePersonal)
	}
}

func (s *Server) handlePutSharedSecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if s.refuseDelegated(w, r) || s.refuseExplicitOwnerParam(w, r, "secret.write", name) {
		return
	}
	s.putSecretFor(w, r, name, "", secretScopeShared)
}

func (s *Server) handleDeleteSharedSecret(w http.ResponseWriter, r *http.Request) {
	name := chi.URLParam(r, "name")
	if s.refuseDelegated(w, r) || s.refuseExplicitOwnerParam(w, r, "secret.delete", name) {
		return
	}
	s.deleteSecretFrom(w, r, name, "", true, secretScopeShared)
}

// handleListMySecrets and handleListSharedSecrets return {"names": [...]}: the caller's own
// namespace, or the shared one. Names only, reserved platform keys excluded, as GET /secrets.
func (s *Server) handleListMySecrets(w http.ResponseWriter, r *http.Request) {
	if refuseListOwnerParam(w, r) {
		return
	}
	if owner, ok := s.personalSecretOwner(w, r); ok {
		s.listNamespace(w, r, owner)
	}
}

func (s *Server) handleListSharedSecrets(w http.ResponseWriter, r *http.Request) {
	if refuseListOwnerParam(w, r) {
		return
	}
	s.listNamespace(w, r, "")
}

// refuseListOwnerParam answers ?owner= on an explicit list route as the write routes do: the route
// names its namespace itself, so a filter that would not apply is refused, not ignored. Reading
// is not audited.
func refuseListOwnerParam(w http.ResponseWriter, r *http.Request) bool {
	if !r.URL.Query().Has("owner") {
		return false
	}
	writeErrorReason(w, http.StatusForbidden, reasonSecretOwnerParamRefused, secretExplicitOwnerRefusal)
	return true
}

func (s *Server) listNamespace(w http.ResponseWriter, r *http.Request, owner string) {
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	names, err := reservedFilteredSecretNames(r.Context(), s.cfg.Secrets.For(owner))
	if err != nil {
		writeServerError(w, r, "list secrets", err)
		return
	}
	window, truncated := pageWindow(names, page.Offset, page.Limit)
	if truncated {
		w.Header().Set("X-Wardyn-Truncated", "true")
	}
	writeJSON(w, http.StatusOK, map[string]any{"names": window})
}

// withScope adds scope to a secret.write/secret.delete row's data; "" (the legacy routes) leaves it as it was.
func withScope(data json.RawMessage, scope string) json.RawMessage {
	if scope == "" {
		return data
	}
	m := map[string]any{}
	if len(data) > 0 {
		_ = json.Unmarshal(data, &m)
	}
	m["scope"] = scope
	return mustJSON(m)
}
