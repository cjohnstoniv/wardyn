// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountDelegationRoutes mounts the portal surface (#1142): the anonymous-at-
// the-router token exchange, which authenticates the portal itself, and the
// portal registry. Registering a portal is the SUPER admin's alone (owner
// decision Q5); listing and revoking follow the device inventory onto the
// security tier, since a revoke only ever subtracts reach.
func (s *Server) mountDelegationRoutes(r chi.Router) {
	r.Post("/token", s.handleTokenExchange)
	r.Group(func(r chi.Router) {
		r.Use(s.humanOrAdminAuth)
		r.With(s.requireOperator).Post("/admin/delegates", s.handleRegisterDelegate)
		securityOps := r.With(s.requireSecurityOperator)
		securityOps.Get("/admin/delegates", s.handleListDelegates)
		securityOps.Delete("/admin/delegates/{id}", s.handleRevokeDelegate)
	})
}

func (s *Server) delegateStoreOr501(w http.ResponseWriter) (store.DelegateStore, bool) {
	ds, ok := s.cfg.Store.(store.DelegateStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "delegation requires the Postgres store backend")
	}
	return ds, ok
}

type registerDelegateRequest struct {
	Name        string `json:"name"`
	IdPClientID string `json:"idp_client_id"`
	Group       string `json:"group"`
}

// handleRegisterDelegate is POST /api/v1/admin/delegates: register one portal
// and return its credential exactly once — the row keeps only its hash.
//
// The group is canonicalized the way a sign-in's group snapshot is
// (oidc.CanonicalGroupSubject), so it matches the exact string a person's
// token produces. The portal's IdP client id may not be this deployment's own:
// a subject token issued to Wardyn would then pass the audience check for it.
func (s *Server) handleRegisterDelegate(w http.ResponseWriter, r *http.Request) {
	ds, ok := s.delegateStoreOr501(w)
	if !ok {
		return
	}
	var req registerDelegateRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" || len(name) > apiTokenNameMaxLen || !controlCharFree(name) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegateNameInvalid, "name: required, at most 200 bytes, no control characters")
		return
	}
	clientID := strings.TrimSpace(req.IdPClientID)
	if clientID == "" || len(clientID) > apiTokenNameMaxLen || !controlCharFree(clientID) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegateClientIDInvalid, "idp_client_id: required, at most 200 bytes, no control characters")
		return
	}
	if s.cfg.OIDC != nil && clientID == s.cfg.OIDC.ClientID() {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegateClientIDIsPortal, "idp_client_id: must be the portal's own client, not this deployment's")
		return
	}
	group, ok := oidc.CanonicalGroupSubject(req.Group)
	if !ok {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonDelegateGroupInvalid, "group: required, printable ASCII")
		return
	}
	raw := newBearer(delegateCredentialPrefix)
	d, err := ds.CreateDelegate(r.Context(), types.Delegate{
		ID: uuid.New(), Name: name, IdPClientID: clientID, Group: group, RegisteredBy: principalFromRequest(r),
	}, raw)
	if err != nil {
		writeServerError(w, r, "register delegate", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"delegate.create", d.ID.String(), "success",
		mustJSON(map[string]any{"name": d.Name, "idp_client_id": d.IdPClientID, "group": d.Group})))
	d.Credential = raw
	writeJSON(w, http.StatusCreated, d)
}

// handleListDelegates is GET /api/v1/admin/delegates: every registered portal,
// revoked ones included, newest first. Never a credential or its hash.
func (s *Server) handleListDelegates(w http.ResponseWriter, r *http.Request) {
	ds, ok := s.delegateStoreOr501(w)
	if !ok {
		return
	}
	list, err := ds.ListDelegates(r.Context())
	if err != nil {
		writeServerError(w, r, "list delegates", err)
		return
	}
	if list == nil {
		list = []types.Delegate{}
	}
	writeJSON(w, http.StatusOK, list)
}

// handleRevokeDelegate is DELETE /api/v1/admin/delegates/{id}: the portal can
// no longer exchange, and every delegated token it holds answers 401 on its
// next request. Runs it launched keep running — they are the person's runs
// (owner decision Q6). Already revoked is 404 and writes no second row.
func (s *Server) handleRevokeDelegate(w http.ResponseWriter, r *http.Request) {
	ds, ok := s.delegateStoreOr501(w)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "delegate")
	if !ok {
		return
	}
	d, err := ds.RevokeDelegate(r.Context(), id, s.cfg.Now().UTC())
	if notFoundIf(w, err, "delegate", reasonDelegateNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "revoke delegate", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"delegate.revoke", d.ID.String(), "success", mustJSON(map[string]any{"name": d.Name})))
	w.WriteHeader(http.StatusNoContent)
}
