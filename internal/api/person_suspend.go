// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bytes"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The admin routes of a portal person's lifecycle: POST /people/{principal}/suspend (security tier),
// /reactivate and /purge (super admin), and the status view. A suspend with a purge deadline is the permanent
// purge scheduled, so that needs the super admin too. They work only on delegate:person: principals; a
// console person gets 422. Because a person is one per issuer and provider id, one suspend reaches them through
// every portal. They are API-only in 0.10.2: the People page shows the state and has no button for them.

// maxPortalPurgeAfter bounds the deadline an admin may give a suspension, so the deadline's arithmetic in the
// database can never overflow.
const maxPortalPurgeAfter = 10 * 365 * 24 * time.Hour

// suspendPortalPersonRequest is the optional body of a suspend.
type suspendPortalPersonRequest struct {
	// PurgeAfterSeconds schedules the permanent purge that long after the suspension; only a super admin may set
	// it above 0, as for an explicit purge. Absent or 0 means no deadline.
	PurgeAfterSeconds int64 `json:"purge_after_seconds"`
}

// portalPersonStateView is a lifecycle route's answer: where the person stands now.
type portalPersonStateView struct {
	Principal     string     `json:"principal"`
	State         string     `json:"state"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
	PurgeAfter    *time.Time `json:"purge_after,omitempty"`
	PurgedAt      *time.Time `json:"purged_at,omitempty"`
}

// denyPortalPerson records a refused lifecycle request, naming why. No identity, job or other row is written.
func (s *Server) denyPortalPerson(r *http.Request, action, target, reason string) {
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, action, target, "denied", mustJSON(map[string]any{"reason": reason})))
}

// portalRefusal is the refusal resolvePortalPerson leaves for a lifecycle request: a denied row of action.
func (s *Server) portalRefusal(r *http.Request, action string) func(principal, reason string) {
	return func(principal, reason string) { s.denyPortalPerson(r, action, principal, reason) }
}

// resolvePortalPerson finds the portal person the {principal} path names, or answers and reports false: 422
// portal_person_only for any other principal, 404 person_not_found for a portal principal no portal has ever
// introduced (nothing is created by asking), and 404 portal_issuer_not_bound for a row whose issuer lost its
// binding. onRefused, when set, leaves the request row of a refused lifecycle request (portalRefusal); a read
// passes nil and leaves none.
func (s *Server) resolvePortalPerson(w http.ResponseWriter, r *http.Request, onRefused func(principal, reason string)) (portalLifecycleStore, store.PrincipalIdentity, bool) {
	principal := strings.TrimSpace(principalParam(r))
	deny := func(reason string) {
		if onRefused != nil {
			onRefused(principal, reason)
		}
	}
	if !types.IsPortalPersonPrincipal(principal) {
		deny(reasonPortalPersonOnly)
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPortalPersonOnly, "principal: this names a person who is not a portal person; only people an issuer portal introduced (delegate:person:...) can be acted on here")
		return nil, store.PrincipalIdentity{}, false
	}
	st, ok := s.portalLifecycleStore()
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonDelegationStoreUnavailable, "issuer portals require the Postgres store backend")
		return nil, store.PrincipalIdentity{}, false
	}
	rows, err := st.IdentitiesByPrincipal(r.Context(), principal)
	if err != nil {
		writeServerError(w, r, "read a portal person", err)
		return nil, store.PrincipalIdentity{}, false
	}
	for _, ident := range rows {
		if !strings.HasPrefix(ident.Issuer, types.PortalPersonPrefix) {
			continue
		}
		if err := s.portalAuthority(r.Context(), ident); err != nil {
			if !errors.Is(err, errNotPortalOwned) {
				writeServerError(w, r, "check a portal person's issuer", err)
				return nil, store.PrincipalIdentity{}, false
			}
			deny(reasonPortalIssuerNotBound)
			writeErrorReason(w, http.StatusNotFound, reasonPortalIssuerNotBound, "this person's issuer has no binding")
			return nil, store.PrincipalIdentity{}, false
		}
		return st, ident, true
	}
	deny(reasonPersonNotFound)
	writeErrorReason(w, http.StatusNotFound, reasonPersonNotFound, "no portal has introduced this person, so there is nothing of theirs to act on")
	return nil, store.PrincipalIdentity{}, false
}

// writePortalPersonState answers a lifecycle route with the person's state, read again.
func (s *Server) writePortalPersonState(w http.ResponseWriter, r *http.Request, st portalLifecycleStore, id uuid.UUID) {
	ident, err := st.GetIdentity(r.Context(), id)
	if err != nil {
		writeServerError(w, r, "read a portal person", err)
		return
	}
	writeJSON(w, http.StatusOK, portalPersonStateView{
		Principal: ident.Principal, State: portalState(ident.DeactivatedAt, ident.PurgedAt),
		DeactivatedAt: utcPtr(ident.DeactivatedAt), PurgeAfter: utcPtr(ident.PurgeAfter), PurgedAt: utcPtr(ident.PurgedAt),
	})
}

// handleSuspendPortalPerson is POST /people/{principal}/suspend, security tier; a positive purge_after_seconds
// needs the super admin, checked before anything is written (403 portal_purge_deadline_operator_only). It writes a person.suspend row,
// then runs the portal's suspension with the caller as the origin: the person's grants are deleted in the same
// transaction as the deactivation, so a grant stops at its next request, and every run of theirs is killed.
// The request answers 503 until every step is done, so a retry finishes it.
func (s *Server) handleSuspendPortalPerson(w http.ResponseWriter, r *http.Request) {
	st, ident, ok := s.resolvePortalPerson(w, r, s.portalRefusal(r, "person.suspend"))
	if !ok {
		return
	}
	var req suspendPortalPersonRequest
	raw, ok := readCappedBody(w, r, maxJSONBody, "request body")
	if !ok {
		return
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		if err := types.DecodeOneJSON(bytes.NewReader(raw), &req, true); err != nil {
			writeErrorReason(w, http.StatusBadRequest, reasonInvalidRequestBody, "invalid JSON body: "+err.Error())
			return
		}
	}
	if req.PurgeAfterSeconds > 0 && !s.isOperator(r.Context()) {
		s.denyPortalPerson(r, "person.suspend", ident.Principal, reasonPortalPurgeDeadlineOperatorOnly)
		writeErrorReason(w, http.StatusForbidden, reasonPortalPurgeDeadlineOperatorOnly, "only a super admin may schedule a purge: suspend without purge_after_seconds, or ask a super admin")
		return
	}
	if req.PurgeAfterSeconds < 0 || req.PurgeAfterSeconds > int64(maxPortalPurgeAfter/time.Second) {
		s.denyPortalPerson(r, "person.suspend", ident.Principal, reasonPortalPersonParamInvalid)
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonPortalPersonParamInvalid, "purge_after_seconds: must be between 0 and ten years; 0 or absent means no deadline")
		return
	}
	purgeAfter := time.Duration(req.PurgeAfterSeconds) * time.Second
	if ident.PurgedAt != nil {
		s.denyPortalPerson(r, "person.suspend", ident.Principal, reasonPersonPurged)
		writeErrorReason(w, http.StatusConflict, reasonPersonPurged, "the person was permanently purged; the tombstone cannot be undone")
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "person.suspend", ident.Principal, "success",
		mustJSON(map[string]any{"identity": ident.ID, "purge_after_seconds": req.PurgeAfterSeconds})))
	switch err := s.runPortalSuspension(r.Context(), st, ident.ID, portalOrigin(actorType, actor), purgeAfter, nil); {
	case errors.Is(err, errDeprovisionIncomplete):
		writeErrorReason(w, http.StatusServiceUnavailable, reasonPortalLifecycleIncomplete, "the suspension is recorded and some of its steps are still pending; retry the request")
		return
	case err != nil:
		writeServerError(w, r, "suspend a portal person", err)
		return
	}
	s.writePortalPersonState(w, r, st, ident.ID)
}

// handleReactivatePortalPerson is POST /people/{principal}/reactivate, super admin: it clears the deactivation
// and any purge deadline, then writes a person.reactivate row. A purged person answers 409: the tombstone is
// permanent.
func (s *Server) handleReactivatePortalPerson(w http.ResponseWriter, r *http.Request) {
	st, ident, ok := s.resolvePortalPerson(w, r, s.portalRefusal(r, "person.reactivate"))
	if !ok {
		return
	}
	reactivated, err := st.ReactivatePortalIdentity(r.Context(), ident.ID)
	if err != nil {
		writeServerError(w, r, "reactivate a portal person", err)
		return
	}
	if !reactivated {
		s.denyPortalPerson(r, "person.reactivate", ident.Principal, reasonPersonPurged)
		writeErrorReason(w, http.StatusConflict, reasonPersonPurged, "the person was permanently purged; the tombstone cannot be undone")
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "person.reactivate", ident.Principal, "success", mustJSON(map[string]any{"identity": ident.ID})))
	s.writePortalPersonState(w, r, st, ident.ID)
}
