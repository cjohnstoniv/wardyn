// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// revokeSessionsRequest is POST /api/v1/sessions/revoke's body: exactly one
// of Sub (revoke a single principal's sessions) or All (revoke every
// principal's sessions) must be set — see handleRevokeSessions.
type revokeSessionsRequest struct {
	Sub string `json:"sub"`
	All bool   `json:"all"`
}

// handleRevokeSessions is D16's admin surface for "revoke a human now" — the
// OIDC session cookie is stateless (see internal/auth/oidc's package doc), so
// there is no session row to delete; instead this stamps a CUTOFF
// (Config.SessionRevocations) that oidc.Authenticator.Middleware checks on
// every authenticated request, so a still-unexpired session for the target
// stops working on its VERY NEXT request rather than lingering until its own
// Expiry (up to the OIDC-configured token lifetime) — the same behavior a
// logout, an IdP role demotion, or an IdP account disablement could not
// otherwise force before this existed.
//
// It also revokes every unrevoked per-user API token the target holds: a
// wdn_ bearer authenticates AS that human (apiTokenAuth) and never consults
// the session cutoff, so leaving it alive would make "revoke a human now" a
// half-measure the operator has to know to finish by hand.
//
// Mounted only when OIDC + SessionRevocations are both wired (see routes.go)
// — with no OIDC session mechanism there is nothing to revoke.
//
// NO TARGET-ROLE GUARD, DELIBERATELY — and this is the SECURITY_ADMIN tier
// (securityOps, routes.go), so a security admin may cut a SUPER ADMIN's
// sessions and tokens, and the All arm below cuts every principal's. Asked
// directly: should a security admin be refused a target in the tier above?
// No, and the reasons are structural rather than a judgement call:
//
//   - Revocation only SUBTRACTS. It hands the caller nothing — no session, no
//     token, no reach — which is the exact property that put this route on
//     securityOps at all. A guard here would protect no capability; it would
//     only decide who may perform an audited subtraction.
//   - Incident response IS this tier's job. RevokeAll's own doc calls it "the
//     incident-response 'log everyone out' lever", and a lever that exempts the
//     most privileged accounts is not one — a compromised super-admin session
//     is precisely the case you buy it for.
//   - The tiers deliberately DO NOT NEST (routes.go's securityOps rationale).
//     "security_admin may not act on admin" would be a ladder assertion, and
//     the ladder is the shape this design refuses; the asymmetry it DOES keep
//     is the one that matters — a security admin's SSH key and attach ticket
//     still stamp member, so this tier never yields a shell in a foreign
//     sandbox.
//   - It is not a lockout, so it cannot be used to hold the deployment. The
//     cutoff is a TIMESTAMP (oidc.SessionRevocations.IsSessionRevoked), so the
//     target signs in again and their new session's issued-at clears it; and
//     adminAuth (http.go) never consults revocations, so the admin bearer
//     break-glass survives even the All arm. A rogue security admin cannot
//     revoke their way to an un-revertible position.
//
// What a guard here WOULD have cost is the case it exists for: a super admin
// whose session is the compromised one, at 3am, with the security admin the
// only person on call.
//
// API tokens are the asymmetric half and the reason the All arm is an incident
// lever rather than a routine one: unlike sessions they do not self-heal, and
// every automation credential in the deployment must be re-minted by hand.
// Pinned by TestSecurityAdminRevokesSuperAdmin; stated for operators in
// docs/OPERATIONS.md's security-admin section.
func (s *Server) handleRevokeSessions(w http.ResponseWriter, r *http.Request) {
	var body revokeSessionsRequest
	if !decodeStrict(w, r, &body) {
		return
	}
	body.Sub = strings.TrimSpace(body.Sub)

	switch {
	case body.All && body.Sub != "":
		writeError(w, http.StatusBadRequest, `body must set exactly one of "sub" or "all", not both`)
		return
	case body.All:
		if err := s.cfg.SessionRevocations.RevokeAll(r.Context()); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke all sessions: "+err.Error())
			return
		}
		n, err := s.revokeAPITokensFor(r.Context(), "")
		if err != nil {
			// The sessions ARE revoked and 0..n tokens with them — a bare 500
			// would hide a partially-applied security action from the
			// append-only log. Record what happened, then fail; the call is
			// idempotent, so a retry converges on whatever is still live.
			s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"session.revoke", "*", "failure", mustJSON(map[string]any{"scope": "all", "tokens_revoked": n, "error": err.Error()})))
			writeError(w, http.StatusInternalServerError, "revoke api tokens: "+err.Error())
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"session.revoke", "*", "success", mustJSON(map[string]any{"scope": "all", "tokens_revoked": n})))
	case body.Sub != "":
		if err := s.cfg.SessionRevocations.RevokeSub(r.Context(), body.Sub); err != nil {
			writeError(w, http.StatusInternalServerError, "revoke sessions: "+err.Error())
			return
		}
		n, err := s.revokeAPITokensFor(r.Context(), body.Sub)
		if err != nil {
			// Same partial-application honesty as the all arm above.
			s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
				"session.revoke", body.Sub, "failure", mustJSON(map[string]any{"scope": "sub", "sub": body.Sub, "tokens_revoked": n, "error": err.Error()})))
			writeError(w, http.StatusInternalServerError, "revoke api tokens: "+err.Error())
			return
		}
		s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
			"session.revoke", body.Sub, "success", mustJSON(map[string]any{"scope": "sub", "sub": body.Sub, "tokens_revoked": n})))
	default:
		writeError(w, http.StatusBadRequest, `body must be {"sub":"<principal>"} or {"all":true}`)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// revokeAPITokensFor revokes every unrevoked API token owned by principal, or
// by everyone when principal is "" (the revoke-all arm). Returns how many it
// revoked. ponytail: one list + a loop over the existing admin-scoped
// RevokeAPIToken rather than a new bulk store method — a principal holds a
// handful of tokens, never thousands.
func (s *Server) revokeAPITokensFor(ctx context.Context, principal string) (int, error) {
	var (
		toks []types.APIToken
		err  error
	)
	if principal == "" {
		toks, err = s.cfg.Store.ListAPITokens(ctx)
	} else {
		toks, err = s.cfg.Store.ListAPITokensByPrincipal(ctx, principal)
	}
	if err != nil {
		return 0, err
	}
	now := time.Now().UTC()
	n := 0
	for _, t := range toks {
		if t.RevokedAt != nil {
			continue
		}
		if _, err := s.cfg.Store.RevokeAPIToken(ctx, t.ID, "", now); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
