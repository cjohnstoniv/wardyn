// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The operator's doors for a provider's service grant: the client secret's route, the grant's own removal,
// the clean-up retry, and the read model GET /model-providers adds. Every route is operator only, like every
// provider write, and takes the site-config document lock across its checks and its store call.

import (
	"context"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// mountServiceGrantRoutes registers the service grant's routes on the operator-only group, beside the provider routes.
func (s *Server) mountServiceGrantRoutes(operatorOnly chi.Router) {
	operatorOnly.Put("/model-providers/{id}/service-grant/secret", s.handlePutServiceGrantSecret)
	operatorOnly.Delete("/model-providers/{id}/service-grant/secret", s.handleDeleteServiceGrantSecret)
	operatorOnly.Delete("/model-providers/{id}/service-grant", s.handleDeleteServiceGrant)
	operatorOnly.Post("/service-grant-cleanups/{uid}/retry", s.handleRetryServiceGrantCleanup)
}

// putServiceGrantSecretRequest is the secret route's body: the value, and the destination the writer saw.
type putServiceGrantSecretRequest struct {
	Value    string `json:"value"`
	TokenURL string `json:"token_url"`
	ClientID string `json:"client_id"`
	BaseURL  string `json:"base_url"`
}

const (
	mpServiceGrantDestination = "The provider's token URL, client id or gateway address is not the one this secret was written for; check them and write the secret again"
)

// serviceGrantRouteRow reads the live row for a route that holds the document lock: 404 for no such provider,
// 409 service_grant_not_configured for one with no grant. ok=false: the answer is written.
func (s *Server) serviceGrantRouteRow(w http.ResponseWriter, r *http.Request, id string) (types.ModelProvider, store.ServiceGrantDoc, bool) {
	sc, err := s.cfg.Store.GetSiteConfig(r.Context())
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return types.ModelProvider{}, store.ServiceGrantDoc{}, false
	}
	p, found := modelProviderByID(sc.ModelProviders, id)
	switch {
	case !found:
		writeErrorReason(w, http.StatusNotFound, reasonModelProviderNotFoundEntity, "no such model provider")
		return p, store.ServiceGrantDoc{}, false
	case p.OIDC == nil || p.OIDC.ServiceGrant == nil:
		writeErrorReason(w, http.StatusConflict, reasonServiceGrantNotConfigured, "provider has no service grant: "+id)
		return p, store.ServiceGrantDoc{}, false
	}
	var doc store.ServiceGrantDoc
	if gs, ok := s.serviceGrantStore(); ok {
		if doc, err = gs.ServiceGrantDoc(r.Context()); err != nil {
			writeServerError(w, r, "read service grants", err)
			return p, doc, false
		}
	}
	return p, doc, true
}

// handlePutServiceGrantSecret stores the client secret as one record with its destination.
//
//	PUT /api/v1/model-providers/{id}/service-grant/secret
func (s *Server) handlePutServiceGrantSecret(w http.ResponseWriter, r *http.Request) {
	var body putServiceGrantSecretRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return
	}
	defer unlock()
	p, doc, ok := s.serviceGrantRouteRow(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	ctx := r.Context()
	g := p.OIDC.ServiceGrant
	_, pending := doc.Cleanups[p.UID]
	switch {
	case body.TokenURL != g.TokenURL || body.ClientID != g.ClientID || body.BaseURL != p.BaseURL:
		s.auditServiceGrantSecret(ctx, r, "model_provider.service_grant.secret_write", p, "failure",
			map[string]any{"reason": reasonServiceGrantDestinationChanged, "body_token_host": gatewayHost(body.TokenURL),
				"token_host": gatewayHost(g.TokenURL), "body_base_host": gatewayHost(body.BaseURL), "base_host": gatewayHost(p.BaseURL)})
		writeErrorReason(w, http.StatusConflict, reasonServiceGrantDestinationChanged, mpServiceGrantDestination)
		return
	case pending:
		writeErrorReason(w, http.StatusConflict, reasonServiceGrantCleanupPending, serviceGrantCleanupPendingMsg(p.ID))
		return
	case !maskableServiceSecret(body.Value):
		writeErrorReason(w, http.StatusBadRequest, reasonSecretValueTooShort,
			"secret too short: must be at least 8 bytes, not counting a trailing line break, to be masked and scanned")
		return
	}
	rec := serviceGrantRecord{V: 1, TokenURL: g.TokenURL, ClientID: g.ClientID, BaseURL: p.BaseURL, ClientSecret: body.Value}
	if err := s.putServiceGrantRecord(ctx, serviceGrantSecretName(p.UID), rec); err != nil {
		writeServerError(w, r, "store the service grant secret", err)
		return
	}
	s.dropServiceGrantCache(p.UID)
	s.auditServiceGrantSecret(ctx, r, "model_provider.service_grant.secret_write", p, "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

// handleDeleteServiceGrantSecret deletes the record and retires every value masked for it.
//
//	DELETE /api/v1/model-providers/{id}/service-grant/secret
func (s *Server) handleDeleteServiceGrantSecret(w http.ResponseWriter, r *http.Request) {
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return
	}
	defer unlock()
	p, _, ok := s.serviceGrantRouteRow(w, r, chi.URLParam(r, "id"))
	if !ok {
		return
	}
	if err := s.deleteServiceGrantRecord(r.Context(), serviceGrantSecretName(p.UID), nil); err != nil {
		writeServerError(w, r, "delete the service grant secret", err)
		return
	}
	s.auditServiceGrantSecret(r.Context(), r, "model_provider.service_grant.secret_delete", p, "success", nil)
	w.WriteHeader(http.StatusNoContent)
}

// auditServiceGrantSecret writes one of the two secret-route actions: the provider id and uid, the client id and
// the hosts of the destination, never the value.
func (s *Server) auditServiceGrantSecret(ctx context.Context, r *http.Request, action string, p types.ModelProvider, outcome string, extra map[string]any) {
	g := p.OIDC.ServiceGrant
	data := map[string]any{"provider": p.ID, "provider_uid": p.UID, "client_id": g.ClientID,
		"token_host": gatewayHost(g.TokenURL), "base_host": gatewayHost(p.BaseURL)}
	for k, v := range extra {
		data[k] = v
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), action, p.ID, outcome, mustJSON(data)))
}

// handleDeleteServiceGrant removes a row's grant and, after the commit, its secret record. 409 when it has none.
//
//	DELETE /api/v1/model-providers/{id}/service-grant
func (s *Server) handleDeleteServiceGrant(w http.ResponseWriter, r *http.Request) {
	r, unlock, ok := s.lockDoor(w, r, db.SiteConfigLockClass)
	if !ok {
		return
	}
	defer unlock()
	ctx := r.Context()
	id := chi.URLParam(r, "id")
	if _, _, ok := s.serviceGrantRouteRow(w, r, id); !ok {
		return
	}
	existing, err := s.cfg.Store.GetSiteConfig(ctx)
	if err != nil {
		writeServerError(w, r, "get site config", err)
		return
	}
	block := withoutServiceGrant(existing.ModelProviders, id)
	plan, refusal, err := s.planServiceGrantWrite(ctx, existing.ModelProviders, block, true, cleanupReasonGrantRemoved)
	if refusal.write(w) {
		return
	}
	if err != nil {
		writeServerError(w, r, "plan the service grant removal", err)
		return
	}
	candidate := existing
	candidate.ModelProviders = block
	saved, err := s.putSiteConfigGrants(ctx, candidate, plan.change)
	if writeSiteConfigSaveError(w, r, err) {
		return
	}
	warnings, cleanup := s.finishServiceGrantWrite(ctx, plan, nil)
	datum := modelProviderAuditData(storedModelProviders(existing), storedModelProviders(saved))
	datum["service_grant_removed"] = []string{id}
	if cleanup != "" {
		datum["secret_cleanup"] = cleanup
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"model_provider.write", "model_providers", "success", mustJSON(datum)))
	if len(warnings) > 0 {
		writeJSON(w, http.StatusOK, map[string]any{"model_provider_warnings": warnings})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// withoutServiceGrant is a copy of block with the grant of provider id removed and every other row as it was.
func withoutServiceGrant(block *types.ModelProviders, id string) *types.ModelProviders {
	if block == nil {
		return nil
	}
	out := *block
	out.Providers = append([]types.ModelProvider(nil), block.Providers...)
	for i := range out.Providers {
		if out.Providers[i].ID == id && out.Providers[i].OIDC != nil {
			o := *out.Providers[i].OIDC
			o.ServiceGrant = nil
			out.Providers[i].OIDC = &o
		}
	}
	return &out
}

// serviceGrantReadFields are GET /model-providers' service_grants and service_grant_cleanups.
func (s *Server) serviceGrantReadFields(ctx context.Context, block *types.ModelProviders) (map[string]types.ServiceGrantStatus, []types.ServiceGrantCleanup, error) {
	grants, err := s.serviceGrantStatuses(ctx, block)
	if err != nil {
		return nil, nil, err
	}
	gs, ok := s.serviceGrantStore()
	if !ok {
		return grants, nil, nil
	}
	doc, err := gs.ServiceGrantDoc(ctx)
	if err != nil {
		return nil, nil, err
	}
	var cleanups []types.ServiceGrantCleanup
	for _, uid := range sortedCleanupUIDs(doc.Cleanups) {
		c := doc.Cleanups[uid]
		cleanups = append(cleanups, types.ServiceGrantCleanup{
			UID: uid, ProviderID: c.ProviderID, Reason: c.Reason, Since: c.Since, ClientSecretRef: serviceGrantSecretName(uid),
		})
	}
	return grants, cleanups, nil
}
