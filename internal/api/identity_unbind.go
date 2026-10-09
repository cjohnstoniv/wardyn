// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// handleUnbindIdentity is POST /api/v1/admin/identities/{id}/unbind (super admin): the remedy for an
// Entra app re-registration, after which every person whose identity row is bound to a pairwise `sub`
// is refused sign-in (store.ErrIdentityBindingMismatch). It clears the row's principal, so the next
// sign-in binds it to the new `sub`; the row keeps its authority epoch, its deactivation columns and
// its SCIM linkage. What the old principal owns (runs, tokens, keys, credentials) stays under it.
// It also cuts the released sub's sessions, and is refused while that principal still holds an API token, SSH
// key or active run: once the row is bound to the new sub the leaver flow no longer reaches the old one, so
// the admin revokes those first. Only a row keyed by tenant and object id can be re-bound: any other is
// found by its principal alone and would be orphaned. A deactivated or purged identity is refused:
// unbinding is not a way back in for a leaver.
func (s *Server) handleUnbindIdentity(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "identity")
	if !ok {
		return
	}
	st, ok := s.cfg.Store.(store.IdentityUnbinder)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonPeopleStoreUnavailable, "unbinding an identity requires the Postgres store backend")
		return
	}
	ident, err := st.GetIdentity(r.Context(), id)
	if notFoundIf(w, err, "identity", reasonIdentityNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read identity", err)
		return
	}
	if ident.DeactivatedAt != nil || ident.PurgedAt != nil {
		writeErrorReason(w, http.StatusConflict, reasonIdentityDeactivated, "this identity is deactivated; unbinding it would not let the person back in")
		return
	}
	if ident.ObjectID == "" {
		writeErrorReason(w, http.StatusConflict, reasonIdentityNotRebindable, "only an identity keyed by tenant and object id can be re-bound")
		return
	}
	if ident.Principal == "" {
		writeErrorReason(w, http.StatusConflict, reasonIdentityNotBound, "this identity is not bound to a principal; the next sign-in binds it")
		return
	}
	var inUse *store.PrincipalInUseError
	err = st.UnbindIdentity(r.Context(), id, ident.Principal)
	switch {
	case notFoundIf(w, err, "identity", reasonIdentityNotFound):
		return
	case errors.Is(err, store.ErrIdentityDeactivated):
		writeErrorReason(w, http.StatusConflict, reasonIdentityDeactivated, "this identity is deactivated; unbinding it would not let the person back in")
		return
	case errors.Is(err, store.ErrIdentityNotRebindable):
		writeErrorReason(w, http.StatusConflict, reasonIdentityNotRebindable, "only an identity keyed by tenant and object id can be re-bound")
		return
	case errors.As(err, &inUse):
		writeErrorReason(w, http.StatusConflict, reasonIdentityPrincipalInUse, fmt.Sprintf(
			"the principal still holds %d API token(s), %d SSH key(s) and %d active run(s); revoke or end them first, because after the unbind nothing sweeps them", inUse.APITokens, inUse.SSHKeys, inUse.ActiveRuns))
		return
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrIdentityRebound):
		writeErrorReason(w, http.StatusConflict, reasonIdentityNotBound, "the identity changed while it was being unbound; read it again")
		return
	case err != nil:
		writeServerError(w, r, "unbind identity", err)
		return
	}
	actorType, actor := actorFromRequest(r)
	s.recordAudit(r.Context(), s.auditEvent(nil, actorType, actor, "identity.unbind", id.String(), "success",
		mustJSON(map[string]any{
			"principal": ident.Principal, "unbound_by": actor, "sessions_cut": 1,
			"api_tokens": 0, "ssh_keys": 0, "active_runs": 0, // held when it committed: the store refuses otherwise
			"issuer": ident.Issuer, "tenant_id": ident.TenantID, "object_id": ident.ObjectID,
		})))
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "unbound_principal": ident.Principal})
}
