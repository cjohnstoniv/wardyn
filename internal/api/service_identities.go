// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Service identities (#1508), the record and the routes: an admin creates a named, sponsored non-human
// principal "service:<name>", and only its sponsor, signed in, mints its tokens. The tokens are ordinary
// api_tokens rows (service_identity_auth.go authenticates them); the sponsor can read and kill nothing
// here that the allow-list does not already give the service itself.
package api

import (
	"cmp"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"time"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

const (
	// serviceTokenMaxTTL is the default and the most a service token's lifetime can be; the deployment's
	// own cap (WARDYN_API_TOKEN_MAX_TTL) applies when it is lower (D5).
	serviceTokenMaxTTL = 90 * 24 * time.Hour
	// serviceDescriptionMaxLen is the description cap, in characters, as the table's check.
	serviceDescriptionMaxLen = 200
)

var serviceNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{2,62}$`)

// serviceNotFoundMsg is the one body a missing name and another person's identity answer with.
const serviceNotFoundMsg = "service identity not found"

// mountServiceIdentityRoutes registers the service identity family: create, suspend and retire on the
// security tier (D17), and the sponsor's own half beside /me/tokens.
func (s *Server) mountServiceIdentityRoutes(r chi.Router, securityOps chi.Router) {
	securityOps.Post("/service-identities", s.handleCreateServiceIdentity)
	securityOps.Get("/service-identities", s.handleListServiceIdentities)
	securityOps.Get("/service-identities/{name}", s.handleGetServiceIdentity)
	securityOps.Patch("/service-identities/{name}", s.handleUpdateServiceIdentity)
	securityOps.Delete("/service-identities/{name}", s.handleRetireServiceIdentity)
	r.Get("/me/service-identities", s.handleListMyServiceIdentities)
	r.Post("/me/service-identities/{name}/tokens", s.handleMintServiceToken)
	r.Get("/me/service-identities/{name}/tokens", s.handleListServiceTokens)
	r.Delete("/me/service-identities/{name}/tokens/{id}", s.handleRevokeServiceToken)
}

// serviceStoreOr409 is the store behind the family, after the local-mode refusal: service identities need
// SSO because their sponsor is a bound person, and local mode has none. 501 on a store without them.
func (s *Server) serviceStoreOr409(w http.ResponseWriter) (store.ServiceIdentityStore, bool) {
	if s.cfg.LocalMode {
		writeErrorReason(w, http.StatusConflict, reasonNeedsSSO, "service identities need single sign-on: local mode has no person to sponsor one")
		return nil, false
	}
	ss, ok := s.cfg.Store.(store.ServiceIdentityStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonPeopleStoreUnavailable, "service identities require the Postgres store backend")
	}
	return ss, ok
}

// withServiceState sets each identity's derived state: active becomes sponsor_inactive when the sponsor's
// session is not live (deactivated by SCIM, or their credentials cut off), the state CheckSession names.
func (s *Server) withServiceState(r *http.Request, sis ...*types.ServiceIdentity) error {
	for _, si := range sis {
		si.State = si.BaseState()
		if si.State != types.ServiceIdentityActive {
			continue
		}
		if s.cfg.SessionRevocations == nil {
			si.State = types.ServiceIdentitySponsorInactive
			continue
		}
		status, err := oidc.CheckSession(r.Context(), s.cfg.SessionRevocations, si.Sponsor, "", s.cfg.Now(), -1)
		if err != nil {
			return err
		}
		if status != oidc.SessionLive {
			si.State = types.ServiceIdentitySponsorInactive
		}
	}
	return nil
}

// serviceIdentityRequest IS the SDK's body (dto_alias_test.go pins it).
type serviceIdentityRequest = client.CreateServiceIdentityRequest

func (s *Server) handleCreateServiceIdentity(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.serviceStoreOr409(w)
	if !ok {
		return
	}
	var req serviceIdentityRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	ctx := r.Context()
	if !serviceNameRE.MatchString(req.Name) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentityNameInvalid, "name: lowercase letters, digits and hyphens, starting with a letter, 3 to 63 characters")
		return
	}
	if !serviceDescriptionOK(w, req.Description) {
		return
	}
	if !s.serviceSponsorShape(req.Sponsor) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentitySponsorInvalid, serviceSponsorMsg)
		return
	}
	userType := cmp.Or(req.UserType, types.UserTypeStandard)
	switch known, err := s.userTypeExists(ctx, userType); {
	case err != nil:
		writeServerError(w, r, "look up user type", err)
		return
	case !known:
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentityUserTypeUnknown, "user_type: no such user type")
		return
	}
	si := types.ServiceIdentity{
		Name: req.Name, Description: req.Description, Sponsor: req.Sponsor, UserType: userType,
		CreatedBy: principalFromRequest(r),
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	if mode == govQueue {
		s.holdServiceIdentityCreate(w, r, si)
		return
	}
	created, err := ss.CreateServiceIdentity(ctx, si)
	if writeServiceCreateError(w, r, err) {
		return
	}
	// The identity is committed: its row is written even when the sponsor's session cannot be read.
	stateErr := s.withServiceState(r, &created)
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"service_identity.create", created.Principal(), "success",
		mustJSON(map[string]any{"sponsor": created.Sponsor, "user_type": created.UserType})))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindServiceIdentity, created.Principal(), "success", nil)
	}
	if stateErr != nil {
		writeServerError(w, r, "read the sponsor's session", stateErr)
		return
	}
	writeJSON(w, http.StatusCreated, created)
}

// writeServiceCreateError answers a CreateServiceIdentity error (a held create's dry run included) and
// reports whether it wrote one.
func writeServiceCreateError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, store.ErrServiceNameTaken):
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityNameTaken, "that name is already used by a service identity or a principal")
	case errors.Is(err, store.ErrServiceSponsorInvalid):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentitySponsorInvalid, serviceSponsorMsg)
	case errors.Is(err, store.ErrServiceUserTypeUnknown):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentityUserTypeUnknown, "user_type: no such user type")
	default:
		writeServerError(w, r, "create service identity", err)
	}
	return true
}

const serviceSponsorMsg = "sponsor: exactly the principal of a signed-in person, as the identity provider names them; an email or an entra: alias is refused"

// serviceSponsorShape is the shape check on a sponsor before the store asks whether a bound person holds
// it; no reserved mechanism principal is ever bound to an identity row, so the store refuses those.
func (s *Server) serviceSponsorShape(sponsor string) bool {
	return validSubject(sponsor) && !isServicePrincipal(sponsor)
}

func serviceDescriptionOK(w http.ResponseWriter, d string) bool {
	if utf8.RuneCountInString(d) > serviceDescriptionMaxLen || !controlCharFree(d) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentityDescriptionInvalid, "description: at most 200 characters, no control characters")
		return false
	}
	return true
}

func (s *Server) handleListServiceIdentities(w http.ResponseWriter, r *http.Request) {
	s.listServiceIdentities(w, r, "")
}

func (s *Server) handleListMyServiceIdentities(w http.ResponseWriter, r *http.Request) {
	sub := oidcHumanFromContext(r.Context())
	if _, ok := s.serviceStoreOr409(w); !ok {
		return
	}
	if sub == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenNoHuman, apiTokenNoHumanRefusal)
		return
	}
	s.listServiceIdentities(w, r, sub)
}

func (s *Server) listServiceIdentities(w http.ResponseWriter, r *http.Request, sponsor string) {
	ss, ok := s.serviceStoreOr409(w)
	if !ok {
		return
	}
	page, ok := parseListPage(w, r, defaultListLimit)
	if !ok {
		return
	}
	servePage(w, r, page, func(p store.Page) ([]types.ServiceIdentity, error) {
		out, err := ss.ListServiceIdentities(r.Context(), sponsor, p)
		if err != nil {
			return nil, err
		}
		for i := range out {
			if err := s.withServiceState(r, &out[i]); err != nil {
				return nil, err
			}
		}
		return out, nil
	}, nil)
}

func (s *Server) handleGetServiceIdentity(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.serviceStoreOr409(w)
	if !ok {
		return
	}
	si, err := ss.GetServiceIdentity(r.Context(), chi.URLParam(r, "name"))
	if notFoundIf(w, err, "service identity", reasonServiceIdentityNotFound) {
		return
	}
	if err == nil {
		err = s.withServiceState(r, &si)
	}
	if err != nil {
		writeServerError(w, r, "read service identity", err)
		return
	}
	writeJSON(w, http.StatusOK, si)
}

// updateServiceIdentityRequest IS the SDK's body (dto_alias_test.go pins it): the fields present change.
type updateServiceIdentityRequest = client.UpdateServiceIdentityRequest

func (s *Server) handleUpdateServiceIdentity(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.serviceStoreOr409(w)
	if !ok {
		return
	}
	var req updateServiceIdentityRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.Description == nil && req.Sponsor == nil && req.Suspended == nil {
		writeErrorReason(w, http.StatusBadRequest, reasonServiceIdentityPatchEmpty, "name at least one of description, sponsor or suspended")
		return
	}
	if req.Description != nil && !serviceDescriptionOK(w, *req.Description) {
		return
	}
	if req.Sponsor != nil && !s.serviceSponsorShape(*req.Sponsor) {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentitySponsorInvalid, serviceSponsorMsg)
		return
	}
	ctx, name := r.Context(), chi.URLParam(r, "name")
	mode := govDirect
	if req.Sponsor != nil && envEnabled(envGovernanceSecondHuman) {
		// A PATCH that changes the sponsor is a governance write, held whole. One that names the current
		// sponsor changes nothing there, so its sponsor is dropped and the rest applies directly: a
		// direct write never moves the sponsor while the switch is on.
		cur, err := ss.GetServiceIdentity(ctx, name)
		if notFoundIf(w, err, "service identity", reasonServiceIdentityNotFound) {
			return
		}
		if err != nil {
			writeServerError(w, r, "read service identity", err)
			return
		}
		if cur.Sponsor == *req.Sponsor {
			req.Sponsor = nil
		} else {
			var ok bool
			if mode, ok = s.governanceWriteMode(w, r); !ok {
				return
			}
			if mode == govQueue {
				s.holdServiceIdentityUpdate(w, r, cur, req)
				return
			}
		}
	}
	changed, err := ss.UpdateServiceIdentity(ctx, name, store.ServiceIdentityChange{
		Description: req.Description, Sponsor: req.Sponsor, Suspended: req.Suspended,
		By: principalFromRequest(r),
	})
	if writeServiceUpdateError(w, r, err) {
		return
	}
	// The change is committed: its row is written even when the sponsor's session cannot be read, with the
	// state the identity's own row gives.
	stateErr := s.withServiceState(r, &changed.After)
	if stateErr != nil {
		changed.After.State = changed.After.BaseState()
	}
	data := serviceUpdateAuditData(changed)
	data["state"] = changed.After.State
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"service_identity.update", changed.After.Principal(), "success", mustJSON(data)))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindServiceIdentity, changed.After.Principal(), "success", nil)
	}
	if stateErr != nil {
		writeServerError(w, r, "read the sponsor's session", stateErr)
		return
	}
	writeJSON(w, http.StatusOK, changed.After)
}

// serviceUpdateAuditData is service_identity.update's data without the state: the tokens revoked, and
// what changed.
func serviceUpdateAuditData(changed store.ServiceIdentityChanged) map[string]any {
	data := map[string]any{"tokens_revoked": changed.TokensRevoked}
	if changed.After.Sponsor != changed.Before.Sponsor {
		data["sponsor_from"], data["sponsor_to"] = changed.Before.Sponsor, changed.After.Sponsor
	}
	if changed.After.Description != changed.Before.Description {
		data["description_changed"] = true
	}
	return data
}

// writeServiceUpdateError answers an UpdateServiceIdentity error (a held change's dry run included) and
// reports whether it wrote one.
func writeServiceUpdateError(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case notFoundIf(w, err, "service identity", reasonServiceIdentityNotFound):
	case errors.Is(err, store.ErrServiceIdentityInactive):
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "the service identity is retired")
	case errors.Is(err, store.ErrServiceSponsorInvalid):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonServiceIdentitySponsorInvalid, serviceSponsorMsg)
	default:
		writeServerError(w, r, "update service identity", err)
	}
	return true
}

func (s *Server) handleRetireServiceIdentity(w http.ResponseWriter, r *http.Request) {
	ss, ok := s.serviceStoreOr409(w)
	if !ok {
		return
	}
	ctx, name := r.Context(), chi.URLParam(r, "name")
	retired, err := ss.RetireServiceIdentity(ctx, name, principalFromRequest(r))
	actorType, actor := actorTypeFromRequest(r), principalFromRequest(r)
	switch {
	case notFoundIf(w, err, "service identity", reasonServiceIdentityNotFound):
		return
	case errors.Is(err, store.ErrServiceIdentityInactive):
		// Already retired: the retire row was written by the call that retired it, and only the erase is
		// left to do. It is idempotent, so a retire whose erase failed is finished by calling DELETE again.
		var gerr error
		if retired.After, gerr = ss.GetServiceIdentity(ctx, name); gerr != nil {
			writeServerError(w, r, "read service identity", gerr)
			return
		}
	case err != nil:
		writeServerError(w, r, "retire service identity", err)
		return
	default:
		s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "service_identity.retire", retired.After.Principal(), "success",
			mustJSON(map[string]any{"tokens_revoked": retired.TokensRevoked})))
	}
	// So do its runs waiting in a pool's queue: a retired identity's run never starts.
	if _, err := s.cancelQueuedRunsOf(ctx, retired.After.Principal(), queueCancelServiceRetired); err != nil {
		writeServerError(w, r, "cancel the service identity's waiting runs", err)
		return
	}
	// The identity's stored credentials go with it, beside the retire row. The retire is already durable,
	// so a failure here is recorded and answered 500, and the same DELETE run again erases them.
	if s.cfg.Secrets != nil {
		principal := retired.After.Principal()
		var rep secretstore.EraseReport
		_, eerr := s.eraseCredentialsScope(ctx, principal, &rep)
		outcome := "success"
		if eerr != nil {
			outcome = "failure"
		}
		s.recordAudit(ctx, s.auditEvent(nil, actorType, actor, "credential.erase", principal, outcome, withSecretOwner(credentialEraseData(rep), principal, true)))
		if eerr != nil {
			writeServerError(w, r, "erase the service identity's credentials", eerr)
			return
		}
	}
	if err := s.withServiceState(r, &retired.After); err != nil {
		writeServerError(w, r, "read the sponsor's session", err)
		return
	}
	writeJSON(w, http.StatusOK, retired.After)
}

// sponsoredIdentity is the sponsor-only read behind the token routes: the caller's own identity, or false
// after the 403 (no signed-in human), or the 404 every non-sponsor gets, byte for byte the missing name's.
func (s *Server) sponsoredIdentity(w http.ResponseWriter, r *http.Request, sub string) (store.ServiceIdentityStore, types.ServiceIdentity, bool) {
	ss, ok := s.serviceStoreOr409(w)
	if !ok {
		return nil, types.ServiceIdentity{}, false
	}
	si, err := ss.GetServiceIdentity(r.Context(), chi.URLParam(r, "name"))
	if errors.Is(err, store.ErrNotFound) || (err == nil && si.Sponsor != sub) {
		writeErrorReason(w, http.StatusNotFound, reasonServiceIdentityNotFound, serviceNotFoundMsg)
		return nil, si, false
	}
	if err != nil {
		writeServerError(w, r, "read service identity", err)
		return nil, si, false
	}
	return ss, si, true
}

// serviceTokenRequest IS the SDK's body (dto_alias_test.go pins it).
type serviceTokenRequest = client.MintServiceTokenRequest

// handleMintServiceToken is POST /api/v1/me/service-identities/{name}/tokens: the sponsor, signed in,
// mints a token of the service identity and gets the plaintext once (D4). It shares the self-mint's
// guards, so a token, the admin token, a portal and member mode are refused as for POST /me/tokens, and
// the late session re-check closes the same window; the row is the service's, not the sponsor's.
func (s *Server) handleMintServiceToken(w http.ResponseWriter, r *http.Request) {
	authorizedAt := s.cfg.Now().UTC() // before the body is read, for the reason handleCreateAPIToken gives
	var req serviceTokenRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	sub, ok := s.mintGuards(w, r)
	if !ok {
		return
	}
	ss, si, ok := s.sponsoredIdentity(w, r, sub)
	if !ok {
		return
	}
	if !si.Usable() {
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this service identity is "+string(si.BaseState())+"; its tokens cannot be minted")
		return
	}
	name, ok := apiTokenName(w, req.Name)
	if !ok {
		return
	}
	lifetime, clampedFrom, ok := s.serviceTokenLifetime(w, req.TTLSeconds)
	if !ok {
		return
	}
	// A sponsor's session can only be checked against a revocation store; without one the token could
	// never authenticate (serviceTokenAuth refuses it), so the mint is refused rather than handed out dead.
	if s.cfg.SessionRevocations == nil {
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this deployment cannot check the sponsor's session, so a service token would never authenticate")
		return
	}
	// An active record whose sponsor session is not live is sponsor_inactive: its tokens would be refused
	// at once, so the mint is refused the way a suspended or retired identity's is.
	if err := s.withServiceState(r, &si); err != nil {
		writeServerError(w, r, "read the sponsor's session", err)
		return
	}
	if si.State == types.ServiceIdentitySponsorInactive {
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this service identity's sponsor is not active; its tokens cannot be minted")
		return
	}
	if !s.mintSessionLive(w, r, sub, authorizedAt) {
		return
	}
	expires := authorizedAt.Add(lifetime)
	row := types.APIToken{
		ID: uuid.New(), Principal: si.Principal(), Role: oidc.RoleUser, UserType: si.UserType, Groups: []string{},
		GroupsTruncated: new(bool), Name: name, CreatedAt: authorizedAt, ExpiresAt: &expires,
	}
	plaintext := newBearer(apiTokenPrefix)
	created, err := ss.CreateServiceToken(guardOwner(r.Context(), sub), si.Name, sub, apiTokenMaxPerPrincipal, row, plaintext)
	switch {
	case errors.Is(err, store.ErrServiceTokenCap):
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonAPITokenCapReached,
			fmt.Sprintf("too many live API tokens (max %d) — revoke one first", apiTokenMaxPerPrincipal))
		return
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonServiceIdentityNotFound, serviceNotFoundMsg)
		return
	case errors.Is(err, store.ErrServiceIdentityInactive):
		writeErrorReason(w, http.StatusConflict, reasonServiceIdentityInactive, "this service identity is no longer active under you; its tokens cannot be minted")
		return
	case err != nil:
		writeServerError(w, r, "create service token", err)
		return
	}
	created.Token = plaintext
	detail := mintAuditDetail(created, clampedFrom, nil)
	detail["service_identity"] = si.Name
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"service_identity.token.create", created.ID.String(), "success", mustJSON(detail)))
	writeJSON(w, http.StatusCreated, created)
}

// serviceTokenLifetime resolves a service token's lifetime: always an expiry, defaulting to and at most 90
// days, or the deployment's cap when that is lower (D5). The second result is the lifetime asked for when
// it was cut down, 0 when it was not.
func (s *Server) serviceTokenLifetime(w http.ResponseWriter, requested int64) (life, clampedFrom time.Duration, ok bool) {
	if requested < 0 || requested > apiTokenMaxTTLSeconds {
		writeErrorReason(w, http.StatusBadRequest, reasonAPITokenTTLInvalid, "ttl_seconds: must be between 0 and a hundred years")
		return 0, 0, false
	}
	limit := serviceTokenMaxTTL
	if c := s.cfg.APITokenMaxTTL; c > 0 && c < limit {
		limit = c
	}
	life = time.Duration(requested) * time.Second
	switch {
	case life == 0:
		return limit, 0, true
	case life > limit:
		return limit, life, true
	}
	return life, 0, true
}

// handleListServiceTokens is GET /api/v1/me/service-identities/{name}/tokens: the identity's tokens, to its
// sponsor only. No row carries the plaintext or the hash.
func (s *Server) handleListServiceTokens(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.sponsorHuman(w, r)
	if !ok {
		return
	}
	if _, si, ok := s.sponsoredIdentity(w, r, sub); ok {
		s.listAPITokensFor(w, r, si.Principal())
	}
}

// handleRevokeServiceToken is DELETE /api/v1/me/service-identities/{name}/tokens/{id}: the sponsor revokes
// one token of the identity. Another identity's token id answers the missing id's 404.
func (s *Server) handleRevokeServiceToken(w http.ResponseWriter, r *http.Request) {
	sub, ok := s.sponsorHuman(w, r)
	if !ok {
		return
	}
	_, si, ok := s.sponsoredIdentity(w, r, sub)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "api token")
	if !ok {
		return
	}
	revoked, err := s.cfg.Store.RevokeAPIToken(r.Context(), id, si.Principal(), s.cfg.Now().UTC())
	if notFoundIf(w, err, "api token", reasonAPITokenNotFoundEntity) {
		return
	}
	if err != nil {
		writeServerError(w, r, "revoke api token", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"token.revoke", revoked.ID.String(), "success",
		mustJSON(map[string]any{"principal": revoked.Principal, "name": revoked.Name, "minted_by": revoked.MintedBy, "service_identity": si.Name})))
	w.WriteHeader(http.StatusNoContent)
}

// sponsorHuman is the signed-in person behind a sponsor-only read or revoke, or false after the 403 the
// self-service routes give a caller with no person (the admin token, local mode answers needs_sso).
func (s *Server) sponsorHuman(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.cfg.LocalMode {
		writeErrorReason(w, http.StatusConflict, reasonNeedsSSO, "service identities need single sign-on: local mode has no person to sponsor one")
		return "", false
	}
	sub := oidcHumanFromContext(r.Context())
	if sub == "" {
		writeErrorReason(w, http.StatusForbidden, reasonAPITokenNoHuman, apiTokenNoHumanRefusal)
		return "", false
	}
	return sub, true
}
