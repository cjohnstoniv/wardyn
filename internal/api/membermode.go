// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// membermode.go — the user view: POST /me/view, the two mint doors it
// closes, and the audit marker every refusal met inside it carries. Renamed
// in 0.8 from "view as member" / POST /me/member-mode (docs/OPERATIONS.md's
// "Renamed in 0.8" appendix; history not rewritten — a pre-0.8 audit row
// still reads auth.member_mode).
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
	"context"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// DRAFT (M2 canon pending)
const (
	// userViewNoHumanRefusal is the 400 for the no-per-human-role lane: the
	// admin token, local mode, and a deployment with no IdP configured at all.
	// All three are ONE shared credential that isOperator answers true for with
	// no session role to demote — so there is genuinely nothing to pause, and
	// pretending otherwise would ship the exact contradiction: a console that
	// says "you are a user" over an API that still says admin.
	userViewNoHumanRefusal = "The user view needs a signed-in SSO human: the admin token, local mode " +
		"and a deployment with no identity provider all use one shared credential with no per-person " +
		"role to pause. Sign in through SSO to use it."
	// userViewMintRefusal is the 409 both credential-mint doors answer with
	// while the view is on. ONE sentence for both, because it is one rule: a
	// credential minted here would be re-stamped with the caller's REAL role at
	// their next sign-in (store.RefreshAPITokenRoles and the SSH-key stamp
	// refresh, both fired by OnLogin), so a "user" token would quietly become
	// an admin one and outlive the view that created it.
	userViewMintRefusal = "Exit the user view to mint a token or register a key."
)

// userViewRequest is POST /me/view's body. View is "user" to enter the view,
// "admin" (or the zero value — an ABSENT/empty body, or one that omits the
// key) to leave it, and answers 200 either way — deliberately, because an
// empty body is what authz_test.go's routeMatrix probe sends and because
// "turn it off" is the request that must never be hard to make. Any other
// value is refused 400. Clean break from the 0.7 boolean `enabled` shape (no
// alias — docs/OPERATIONS.md's "Renamed in 0.8" appendix).
type userViewRequest struct {
	View string `json:"view"`
	// NoCredential selects the "view as a NEW user (not signed in)" posture —
	// see membermode_preview.go. Meaningful only with View:"user", and stored
	// as the AND of the two, so there is no body that turns the view off and
	// leaves the preview on.
	//
	// decodeStrict sets DisallowUnknownFields (helpers.go), so an old console
	// POSTing this key to an older replica gets a 400 and does NOT enter the
	// view — the fail-safe direction during a rolling upgrade, and the reason
	// the console sends the key only when it is true.
	NoCredential bool `json:"no_credential"`
}

// handleSetUserView is POST /api/v1/me/view.
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
func (s *Server) handleSetUserView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := oidcHumanFromContext(ctx)
	if sub == "" || s.cfg.OIDC == nil {
		writeError(w, http.StatusBadRequest, userViewNoHumanRefusal)
		return
	}
	var req userViewRequest
	// An empty body is not an error here: http.NoBody decodes to the zero
	// struct, whose View is "" — off, the same as View:"admin".
	//
	// `!= 0`, never `> 0` (record.go's optional-body handler is the precedent):
	// a chunked or otherwise unknown-length body reports ContentLength == -1, and
	// `> 0` would skip the decode and silently answer 200 {"user_view":false}
	// to a request that asked to ENTER the view.
	if r.ContentLength != 0 && !decodeStrict(w, r, &req) {
		return
	}
	var on bool
	switch req.View {
	case "", "admin":
		on = false
	case "user":
		on = true
	default:
		writeError(w, http.StatusBadRequest, `"view" must be "user" or "admin"`)
		return
	}
	// The posture is granted by the server, never taken from the body. The
	// no-credential preview only does anything where the model-access agent's
	// roster row is per_user (userPreviewApplies, membermode_preview.go);
	// asking for it anywhere else would enter a view whose banner asserts a
	// state the same deployment immediately contradicts. On `shared` this
	// silently downgrades to the plain view — the honest answer, and no new
	// string.
	preview := on && req.NoCredential && s.userPreviewApplies(ctx, r)
	realRole, err := s.cfg.OIDC.SetUserView(w, r, on, preview)
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
	// whole value of the view over a shared second login is that the trail
	// still names the person. real_role is the STAMPED role SetUserView read
	// off the cookie — never oidcRoleFromContext, which is already clamped to
	// user while the view is on and would record the wrong tier on the
	// turning-OFF row (and would flatten a security_admin into an admin).
	//
	// no_credential rides the datum ONLY when the view was turned ON with the
	// preview posture asked for: every row a pre-0.8 deployment could write
	// stays byte-identical, and the key is a MARKER of which posture was
	// entered rather than a field every row answers — the same rule
	// authzDeniedDatum's user_view marker below follows.
	// noCred is what the SESSION now carries, never what the body asked for.
	// Besides the roster gate above it drops the REAL-USER case: SetUserView
	// writes that caller no cookie at all, so echoing their request would report
	// — and audit — a posture nobody is in, and would make this row's own
	// AUDIT-ACTIONS sentence ("only on a row that turned the view ON with it")
	// false.
	noCred := preview && realRole != oidc.RoleUser
	datum := map[string]any{"enabled": on, "real_role": realRole}
	if noCred {
		datum["no_credential"] = true
	}
	// Dual-emit for one minor (0.8.x, OD-18): every toggle writes BOTH the new
	// action name and, for SIEM stability, the old one it replaces — same
	// datum, so a dashboard still filtering on auth.member_mode keeps seeing
	// rows until it is repointed. docs/OPERATIONS.md's "Renamed in 0.8"
	// appendix says so; removed in 0.9.
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"auth.user_view", "/api/v1/me/view", "success", mustJSON(datum)))
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"auth.member_mode", "/api/v1/me/view", "success", mustJSON(datum)))
	writeJSON(w, http.StatusOK, map[string]any{
		"user_view": on, "user_view_no_credential": noCred,
	})
}

// authzDeniedDatum builds the `authz.denied` Data map, marking the refusals met
// INSIDE the user view so a denial stream reads as "an admin exercising the user
// path" rather than as an incident. The key is OMITTED when the view is off — it
// is a marker, not a field every row has to answer. Renamed in 0.8 from
// member_mode (docs/OPERATIONS.md's "Renamed in 0.8" appendix; history not
// rewritten — no dual-emit here, unlike the auth.user_view action above: this
// key lives inside authz.denied's own Data map, not on a separate audit row,
// so there is no old-action row to keep landing).
//
// Every admin-tier refusal goes through this, not only the two middleware
// chokepoints (requireOperator and requireSecurityOperator, http.go). The
// IN-HANDLER refusals go through it too — getWorkspaceAuthorized's
// operator-owned-workspace arm (helpers.go), secrets.go's ?owner= gate,
// resolveAlwaysTarget's operator-only `always` scope (approvals.go) and
// denyUserField, which carries the workspaces.llm_cred admin-tier arm
// (runs_create_validate.go). They exist precisely so the audit trail does not
// depend on WHERE a refusal happens to live, and secrets.go says so in a
// shape-identity comment; a marker present at only some of the sites would make
// the field unreliable for the one reader it was added for — an operator
// filtering the denial stream to tell an admin walking the member path from a
// member incident.
//
// A hand-rolled map at an admin-tier emit is the regression to look for: the
// two refusals above were exactly that, and each was reachable by
// an admin in member mode doing what the member Getting Started card invites —
// deciding their own run's held egress at scope `always`, creating a workspace.
// authz_denied_doc_test.go's scanner reads this call, so a reason introduced
// here is still held to the published enum.
func authzDeniedDatum(ctx context.Context, reason, method string) map[string]any {
	d := map[string]any{"reason": reason, "method": method}
	if oidc.MemberModeFromContext(ctx) {
		d["user_view"] = true
	}
	return d
}
