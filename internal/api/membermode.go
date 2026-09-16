// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// membermode.go — "view as member" (v0.7.4, field-report P2): POST
// /me/member-mode, the two mint doors it closes, and the audit marker every
// refusal met inside the mode carries.
//
// WHAT THE MODE IS. An admin asks to be treated as a member for the rest of
// this browser session. internal/auth/oidc does the whole of the clamping:
// Session.MemberMode rides the existing cookie and contextWithPrincipal — the
// one read of the stamped role — publishes RoleMember instead. Nothing in this
// package re-derives a tier, so every predicate here (isOperator,
// isSecurityOperator, every ownsRunOrAdmin) follows for free.
//
// WHAT IT IS NOT. It is not impersonation and it is not a second identity: the
// sub, email, groups, ownership and every audit row stay the admin's own. It is
// also not proof that a MEMBER would be refused — a real second identity is
// that proof (docs/OPERATIONS.md names both, and the mode's three ceilings).

import (
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// DRAFT (M2 canon pending)
const (
	// memberModeNoHumanRefusal is the 400 for the no-per-human-role lane: the
	// admin token, local mode, and a deployment with no IdP configured at all.
	// All three are ONE shared credential that isOperator answers true for with
	// no session role to demote — so there is genuinely nothing to pause, and
	// pretending otherwise would ship the exact contradiction P2 names (a
	// console that says "you are a member" over an API that still says admin).
	memberModeNoHumanRefusal = "member mode needs a signed-in SSO human: the admin token, local mode " +
		"and a deployment with no identity provider all use one shared credential with no per-person " +
		"role to pause. Sign in through SSO to use it."
	// memberModeMintRefusal is the 409 both credential-mint doors answer with
	// while the mode is on. ONE sentence for both, because it is one rule: a
	// credential minted here would be re-stamped with the caller's REAL role at
	// their next sign-in (store.RefreshAPITokenRoles and the SSH-key stamp
	// refresh, both fired by OnLogin), so a "member" token would quietly become
	// an admin one and outlive the mode that created it.
	memberModeMintRefusal = "Exit member mode to mint a token or register a key."
)

// memberModeRequest is POST /me/member-mode's body. An ABSENT/empty body
// decodes to enabled:false and answers 200 — deliberately, because that is what
// authz_test.go's routeMatrix probe sends and because "turn it off" is the
// request that must never be hard to make.
type memberModeRequest struct {
	Enabled bool `json:"enabled"`
}

// handleSetMemberMode is POST /api/v1/me/member-mode.
//
// Registered on the PLAIN authenticated router group beside POST /me/ssh-keys,
// never on operatorOnly: inside the mode the caller's effective role IS member,
// so an operator-gated exit would be a door that locks from the inside. A real
// member toggling ON is a no-op 200 for the same reason — they are already what
// they asked to be, and a 4xx would only teach them the control exists for
// someone else.
//
// GUARD ORDER IS LOAD-BEARING. The no-human arm runs FIRST and covers three
// cases at once — admin token, local mode, and s.cfg.OIDC == nil (the ordinary
// shape of a token/local deployment, server.go) — so s.cfg.OIDC is never
// dereferenced on a deployment that has none.
func (s *Server) handleSetMemberMode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := oidcHumanFromContext(ctx)
	if sub == "" {
		writeError(w, http.StatusBadRequest, memberModeNoHumanRefusal)
		return
	}
	var req memberModeRequest
	// An empty body is not an error here: http.NoBody decodes to the zero
	// struct, which is "off".
	if r.ContentLength > 0 && !decodeStrict(w, r, &req) {
		return
	}
	realRole, err := s.cfg.OIDC.SetMemberMode(w, r, req.Enabled)
	if err != nil {
		// decodeSession's own errors: the cookie went missing or stopped
		// verifying between the middleware and here. Not a 500 — there is
		// nothing wrong with the server, the caller simply has no session left
		// to re-sign.
		writeError(w, http.StatusUnauthorized, "no session to change: sign in again")
		return
	}
	// Actor is the admin's OWN sub, from the untouched actorFromRequest: the
	// whole value of the mode over a shared second login is that the trail
	// still names the person. real_role is the STAMPED role SetMemberMode read
	// off the cookie — never oidcRoleFromContext, which is already clamped to
	// member while the mode is on and would record the wrong tier on the
	// turning-OFF row (and would flatten a security_admin into an admin).
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"auth.member_mode", "/api/v1/me/member-mode", "success",
		mustJSON(map[string]any{"enabled": req.Enabled, "real_role": realRole})))
	writeJSON(w, http.StatusOK, map[string]any{"member_mode": req.Enabled})
}

// authzDeniedDatum builds the `authz.denied` Data map, marking the refusals met
// INSIDE member mode so a denial stream reads as "an admin exercising the member
// path" rather than as an incident. The key is OMITTED when the mode is off — it
// is a marker, not a field every row has to answer.
func authzDeniedDatum(ctx context.Context, reason, method string) map[string]any {
	d := map[string]any{"reason": reason, "method": method}
	if oidc.MemberModeFromContext(ctx) {
		d["member_mode"] = true
	}
	return d
}
