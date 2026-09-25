// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// membermode.go — "view as member": POST
// /me/member-mode, the mint door it closes, and the audit marker every
// refusal met inside the mode carries.
//
// What the mode is. An admin asks to be treated as a member for the rest of
// this browser session. internal/auth/oidc does the whole of the clamping:
// Session.MemberMode rides the existing cookie and contextWithPrincipal — the
// one read of the stamped role — publishes RoleUser instead. Nothing in this
// package re-derives a tier, so every predicate here (isOperator,
// isSecurityOperator, every ownsRunOrAdmin) follows for free.
//
// What it is not. It is not impersonation and it is not a second identity: the
// sub, email, groups, ownership and every audit row stay the admin's own. It is
// also not proof that a MEMBER would be refused — a real second identity is
// that proof (docs/OPERATIONS.md names both, and the mode's three ceilings).

import (
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// DRAFT (M2 canon pending)
const (
	// memberModeNoHumanRefusal is the 400 for the no-per-human-role lane: the
	// admin token, local mode, and a deployment with no IdP configured at all.
	// All three are ONE shared credential that isOperator answers true for with
	// no session role to demote — so there is genuinely nothing to pause, and
	// pretending otherwise would ship the exact contradiction: a console that
	// says "you are a member" over an API that still says admin.
	memberModeNoHumanRefusal = "member mode needs a signed-in SSO human: the admin token, local mode " +
		"and a deployment with no identity provider all use one shared credential with no per-person " +
		"role to pause. Sign in through SSO to use it."
	// memberModeMintRefusal is the 409 the API-token mint door answers with
	// while the mode is on: a token minted here would be re-stamped with the
	// caller's REAL role at their next sign-in (store.RefreshAPITokenIdentity,
	// fired by OnLogin), so a "user" token would quietly become an admin one
	// and outlive the mode that created it. The SSH-key door no longer refuses:
	// a key registered in the mode is stored capped instead (sshkeys.go,
	// migration 0070).
	memberModeMintRefusal = "Exit member mode to mint a token."
)

// memberModeRequest is POST /me/member-mode's body. An ABSENT/empty body
// decodes to enabled:false and answers 200 — deliberately, because that is what
// authz_test.go's routeMatrix probe sends and because "turn it off" is the
// request that must never be hard to make.
type memberModeRequest struct {
	Enabled bool `json:"enabled"`
	// NoCredential selects the "view as a NEW member (not signed in)"
	// posture — see membermode_preview.go. Meaningful only with Enabled, and
	// stored as the AND of the two, so there is no body that turns the mode off
	// and leaves the preview on.
	//
	// decodeStrict sets DisallowUnknownFields (helpers.go), so a 0.7.5 console
	// POSTing this key to a 0.7.4 replica gets a 400 and does NOT enter the mode
	// — the fail-safe direction during a rolling upgrade, and the reason the
	// console sends the key only when it is true.
	NoCredential bool `json:"no_credential"`
}

// handleSetMemberMode is POST /api/v1/me/member-mode.
//
// Registered on the PLAIN authenticated router group beside POST /me/ssh-keys,
// never on operatorOnly: inside the mode the caller's effective role IS member,
// so an operator-gated exit would be a door that locks from the inside. A real
// member toggling ON is a no-op 200 for the same reason — they are already what
// they asked to be, and a 4xx would only teach them the control exists for
// someone else. It is a no-op in FACT as well as in status code: SetUserView
// writes no cookie for that caller, because everything downstream reads
// the FLAG rather than the stamped tier — a member carrying mm:1 would be shown
// a banner naming an admin role they do not hold and refused their own
// credential mints by the two doors below.
//
// Guard order is load-bearing, and it is TWO conditions, not one. The obvious
// arm is "no per-human identity": the admin token and local mode are one shared
// credential that isOperator answers true for with no session role to demote.
//
// `s.cfg.OIDC == nil` is a SEPARATE condition and cannot be folded into the
// first. The `wdn_`
// API-token lane publishes a human identity through the same withHumanIdentity
// the SSO branch uses (apitokens.go), and http.go mounts that lane REGARDLESS of
// OIDC — deliberately, so an operator who never configured SSO can still hold a
// personal token. So `oidcHumanFromContext(ctx) != ""` is reachable with a nil
// *Authenticator, and a token caller who also attaches any `wardyn_session`
// cookie at all would reach sessionHMAC on it: a nil deref, caught by chi's
// Recoverer as a 500 plus a stack dump per request. Both conditions, one arm,
// one sentence — there is nothing to pause in either case.
func (s *Server) handleSetMemberMode(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := oidcHumanFromContext(ctx)
	if sub == "" || s.cfg.OIDC == nil {
		writeError(w, http.StatusBadRequest, memberModeNoHumanRefusal)
		return
	}
	var req memberModeRequest
	// An empty body is not an error here: http.NoBody decodes to the zero
	// struct, which is "off".
	//
	// `!= 0`, never `> 0` (record.go's optional-body handler is the precedent):
	// a chunked or otherwise unknown-length body reports ContentLength == -1, and
	// `> 0` would skip the decode and silently answer 200 {"member_mode":false}
	// to a request that asked to ENTER the mode.
	if r.ContentLength != 0 && !decodeStrict(w, r, &req) {
		return
	}
	// The posture is granted by the server, never taken from the body. The
	// no-credential preview only does anything where the model-access agent's
	// roster row is per_user (memberPreviewApplies, membermode_preview.go);
	// asking for it anywhere else would enter a mode whose banner asserts a
	// state the same deployment immediately contradicts. On `shared` this
	// silently downgrades to the plain mode — the honest answer, and no new
	// string.
	preview := req.Enabled && req.NoCredential && s.memberPreviewApplies(ctx, r)
	// This route picks no type: the view looks through the same default POST
	// /me/view falls back to.
	typeID := ""
	if req.Enabled {
		var err error
		if typeID, _, err = s.userViewType(ctx, sub, ""); err != nil {
			writeServerError(w, r, "resolve user view type", err)
			return
		}
	}
	realRole, err := s.cfg.OIDC.SetUserView(w, r, req.Enabled, typeID, preview)
	if err != nil {
		// decodeSession's own errors: the cookie went missing or stopped
		// verifying between the middleware and here. Not a 500 — there is
		// nothing wrong with the server, the caller simply has no session left
		// to re-sign.
		//
		// Defence in depth rather than a live lane: Middleware publishes no
		// principal for an expired, revoked or tampered cookie, so those
		// requests land on the no-human 400 above and never get here. Kept
		// because the guard above proves the caller HAS a human identity, not
		// that it came from a cookie this Authenticator can re-sign.
		writeError(w, http.StatusUnauthorized, "no session to change: sign in again")
		return
	}
	// Actor is the admin's OWN sub, from the untouched actorFromRequest: the
	// whole value of the mode over a shared second login is that the trail
	// still names the person. real_role is the STAMPED role SetUserView read
	// off the cookie — never oidcRoleFromContext, which is already clamped to
	// member while the mode is on and would record the wrong tier on the
	// turning-OFF row (and would flatten a security_admin into an admin).
	//
	// no_credential rides the datum ONLY when the mode was turned ON with the
	// preview posture asked for: every row a 0.7.4 deployment could write stays
	// byte-identical, and the key is a MARKER of which posture was entered
	// rather than a field every row answers — the same rule authz.Datum's
	// member_mode marker follows.
	// noCred is what the SESSION now carries, never what the body asked for.
	// Besides the roster gate above it drops the REAL-MEMBER case: SetUserView
	// writes that caller no cookie at all, so echoing their request would report
	// — and audit — a posture nobody is in, and would make this row's own
	// AUDIT-ACTIONS sentence ("only on a row that turned the mode ON with it")
	// false.
	noCred := preview && realRole != oidc.RoleUser
	datum := map[string]any{"enabled": req.Enabled, "real_role": realRole}
	if noCred {
		datum["no_credential"] = true
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"auth.member_mode", "/api/v1/me/member-mode", "success", mustJSON(datum)))
	writeJSON(w, http.StatusOK, map[string]any{
		"member_mode": req.Enabled, "member_mode_no_credential": noCred,
	})
}
