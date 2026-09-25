// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// user_view.go — the user view looks through a chosen user type (user-types
// design §2.7): POST /me/view picks the type, the SSO lane re-checks it on
// every request, and a type deleted mid-session refuses the request rather
// than answering as the admin.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// userViewTypePref is the principal_prefs key holding the admin's last chosen
// type, so the switch preselects it on any device.
const userViewTypePref = "user_view.type"

// userViewRequest is POST /me/view's body. View is "user" to enter the view,
// "admin" (or the zero value — an ABSENT/empty body, or one that omits the
// key) to leave it, and answers 200 either way — deliberately, because an
// empty body is what authz_test.go's routeMatrix probe sends and because
// "turn it off" is the request that must never be hard to make. Any other
// value is refused 400. Clean break from the 0.7 boolean `enabled` shape (no
// alias — docs/OPERATIONS.md's "Renamed in 0.8" appendix).
type userViewRequest struct {
	View string `json:"view"`
	// UserType is the type to view as; empty means the previous choice, then
	// the admin's own type, then the built-in type. Meaningful only with
	// View:"user".
	UserType string `json:"user_type"`
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
// never on operatorOnly: inside the view the caller's effective role IS user,
// so an operator-gated exit would be a door that locks from the inside. A real
// user toggling ON is a no-op 200 for the same reason — they are already what
// they asked to be, and a 4xx would only teach them the control exists for
// someone else. It is a no-op in FACT as well as in status code: SetUserView
// writes no cookie for that caller, because everything downstream reads the
// FLAG rather than the stamped tier — a user carrying mm:1 would be shown a
// banner naming an admin role they do not hold and refused their own
// credential mint by the door below.
//
// Guard order is load-bearing, and it is TWO conditions, not one. The obvious
// arm is "no per-human identity": the admin token and local mode are one
// shared credential that isOperator answers true for with no session role to
// demote.
//
// `s.cfg.OIDC == nil` is a SEPARATE condition and cannot be folded into the
// first. The `wdn_` API-token lane publishes a human identity through the
// same withHumanIdentity the SSO branch uses (apitokens.go), and http.go
// mounts that lane REGARDLESS of OIDC — deliberately, so an operator who
// never configured SSO can still hold a personal token. So
// `oidcHumanFromContext(ctx) != ""` is reachable with a nil *Authenticator,
// and a token caller who also attaches any `wardyn_session` cookie at all
// would reach sessionHMAC on it: a nil deref, caught by chi's Recoverer as a
// 500 plus a stack dump per request. Both conditions, one arm, one sentence —
// there is nothing to pause in either case.
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
	// `!= 0`, never `> 0` (record.go's optional-body handler is the
	// precedent): a chunked or otherwise unknown-length body reports
	// ContentLength == -1, and `> 0` would skip the decode and silently
	// answer 200 {"user_view":false} to a request that asked to ENTER the
	// view.
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
		writeError(w, http.StatusBadRequest, `The "view" field must be "user" or "admin".`)
		return
	}
	typeID := ""
	if on {
		var msg string
		var err error
		typeID, msg, err = s.userViewType(ctx, sub, req.UserType)
		if err != nil {
			writeServerError(w, r, "resolve user view type", err)
			return
		}
		if msg != "" {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
	}
	// The posture is granted by the server, never taken from the body. The
	// no-credential preview only does anything where the model-access agent's
	// roster row is per_user (userPreviewApplies, membermode_preview.go);
	// asking for it anywhere else would enter a view whose banner asserts a
	// state the same deployment immediately contradicts. On `shared` this
	// silently downgrades to the plain view — the honest answer, and no new
	// string.
	preview := on && req.NoCredential && s.userPreviewApplies(ctx, r)
	realRole, err := s.cfg.OIDC.SetUserView(w, r, on, typeID, preview)
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
	// A real user asking for the user view is already in it: SetUserView
	// wrote nothing, so nothing is remembered, audited as entered, or
	// reported — the REAL-USER case AUDIT-ACTIONS' "only on a row that turned
	// the view ON with it" sentence describes.
	entered := on && realRole != oidc.RoleUser
	if entered {
		s.rememberUserViewType(ctx, sub, typeID)
	}
	// no_credential rides the datum ONLY when the view was turned ON with the
	// preview posture asked for: every row a pre-0.8 deployment could write
	// stays byte-identical, and the key is a MARKER of which posture was
	// entered rather than a field every row answers — the same rule
	// internal/authz.Datum's user_view marker follows.
	datum := map[string]any{"enabled": on, "real_role": realRole}
	if entered {
		datum["user_type"] = typeID
		if preview {
			datum["no_credential"] = true
		}
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
	resp := map[string]any{"user_view": entered, "user_type": nil, "user_view_no_credential": entered && preview}
	if entered {
		resp["user_type"] = typeID
	}
	writeJSON(w, http.StatusOK, resp)
}

// userViewType picks the type a view looks through. An asked-for type must
// exist (a 400 sentence otherwise). With none asked for, the first of the
// remembered choice, the admin's own type and the built-in type that still
// exists wins. A store failure is an error, never a quiet fallback.
func (s *Server) userViewType(ctx context.Context, sub, asked string) (id, refusal string, err error) {
	if asked != "" {
		ok, err := s.userTypeExists(ctx, asked)
		if err != nil || ok {
			return asked, "", err
		}
		return "", fmt.Sprintf("There is no user type %q.", asked), nil
	}
	for _, id := range []string{s.rememberedUserViewType(ctx, sub), oidc.StampedUserTypeFromContext(ctx)} {
		if id == "" {
			continue
		}
		ok, err := s.userTypeExists(ctx, id)
		if err != nil || ok {
			return id, "", err
		}
	}
	return types.UserTypeStandard, "", nil
}

func (s *Server) userTypeExists(ctx context.Context, id string) (bool, error) {
	if id == types.UserTypeStandard {
		return true, nil
	}
	if s.cfg.Store == nil {
		return false, errors.New("api: no store configured")
	}
	_, err := s.cfg.Store.GetUserType(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// rememberedUserViewType reads the principal's last chosen type, "" when there
// is none or it cannot be read. It is a default, not a control, so a failed
// read falls through to the next candidate.
func (s *Server) rememberedUserViewType(ctx context.Context, sub string) string {
	ps, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok {
		return ""
	}
	raw, err := ps.GetPrincipalPref(ctx, sub, userViewTypePref)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			slog.WarnContext(ctx, "api: could not read the remembered user view type", "error", err)
		}
		return ""
	}
	var id string
	if json.Unmarshal(raw, &id) != nil {
		return ""
	}
	return id
}

func (s *Server) rememberUserViewType(ctx context.Context, sub, typeID string) {
	ps, ok := s.cfg.Store.(store.PrincipalPrefStore)
	if !ok {
		return
	}
	if err := ps.PutPrincipalPref(ctx, sub, userViewTypePref, mustJSON(typeID)); err != nil {
		slog.WarnContext(ctx, "api: could not remember the user view type", "error", err)
	}
}

// userViewTypeDeleted is the refusal for a request made in a user view whose
// type has been deleted; the launch doors answer 409 with the S1 reason.
func userViewTypeDeleted(typeID string) string {
	return fmt.Sprintf("The %s user type was removed, so you're back in the Admin view. Choose another type to use the User view.", typeID)
}

func userViewLaunchRefusal(typeID string) string {
	return fmt.Sprintf("The %s user type was removed, so you're back in the Admin view. Switch to the User view to launch.", typeID)
}

// userViewGate re-checks the type a user-view session looks through, once per
// request and before anything reads the tier. A deleted type turns the view
// off on the cookie and refuses the request (403 user_view_type_deleted; the
// launch doors 409 admin_view). The request is never re-evaluated as the
// admin: its tier was read as user, and an admin-tier answer halfway through
// would apply the operator exemption to a request admitted as a user.
//
// Two exceptions. GET /me is the one request that drops back and answers the
// admin's real tier, carrying user_view_dropped. The switch routes pass
// untouched, so the way out and "choose another type" always work.
//
// It returns the request to serve, or nil when it answered.
func (s *Server) userViewGate(w http.ResponseWriter, r *http.Request) *http.Request {
	ctx := r.Context()
	if !oidc.MemberModeFromContext(ctx) {
		return r
	}
	if r.URL.Path == "/api/v1/me/view" {
		return r
	}
	typeID := oidc.UserTypeFromContext(ctx)
	ok, err := s.userTypeExists(ctx, typeID)
	if err != nil {
		writeServerError(w, r, "resolve user view type", err)
		return nil
	}
	if ok {
		return r
	}
	dropped, err := s.cfg.OIDC.DropUserView(w, r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "no session to change: sign in again")
		return nil
	}
	if r.Method == http.MethodGet && r.URL.Path == "/api/v1/me" {
		s.recordAudit(ctx, s.auditEvent(nil, types.ActorHuman, oidc.PrincipalFromContext(ctx),
			"auth.user_view", "/api/v1/me", "success",
			mustJSON(map[string]any{"enabled": false, "user_type": typeID, "reason": "user_type_deleted"})))
		return r.WithContext(dropped)
	}
	// Written here rather than through refuse: the gate runs before the api's
	// human is published, and the body carries its reason code (409 admin_view
	// on a launch door, where the row still records user_view_type_deleted).
	d := authz.Deny(authz.ReasonUserViewTypeDeleted, r.URL.Path, userViewTypeDeleted(typeID))
	if r.Method == http.MethodPost && (r.URL.Path == "/api/v1/runs" || r.URL.Path == "/api/v1/runs/preflight") {
		s.recordAudit(ctx, s.refusalEvent(ctx, types.ActorHuman, oidc.PrincipalFromContext(ctx), r.Method,
			d.With("answered", "admin_view")))
		launch := authz.Deny(authz.ReasonAdminView, r.URL.Path, userViewLaunchRefusal(typeID))
		writeJSON(w, launch.Status, errorBody{Error: launch.Sentence, Reason: string(launch.Reason)})
		return nil
	}
	s.recordAudit(ctx, s.refusalEvent(ctx, types.ActorHuman, oidc.PrincipalFromContext(ctx), r.Method, d))
	writeJSON(w, d.Status, errorBody{Error: d.Sentence, Reason: string(d.Reason)})
	return nil
}

// meUserViewDropped is /me's user_view_dropped: the type whose deletion
// turned this session's user view off, until the next switch. nil otherwise.
func meUserViewDropped(ctx context.Context) any {
	id := oidc.UserViewDroppedFromContext(ctx)
	if id == "" {
		return nil
	}
	return map[string]string{"user_type": id, "reason": "deleted"}
}

// runCreatorUserType is the type a run is frozen with (AgentRun.UserType): the
// type the creator resolves as, the built-in one for a human with no stamp
// (callerSubjects' rule), "" when no human created it.
func runCreatorUserType(ctx context.Context) string {
	if oidcHumanFromContext(ctx) == "" {
		return ""
	}
	if t := oidcUserTypeFromContext(ctx); t != "" {
		return t
	}
	return types.UserTypeStandard
}

// withRunUserType adds the run.create datum's user_type, and the user_view
// marker for a run launched in the user view.
func withRunUserType(ctx context.Context, userType string, data map[string]any) map[string]any {
	if userType != "" {
		data["user_type"] = userType
	}
	if oidc.MemberModeFromContext(ctx) {
		data["user_view"] = true
	}
	return data
}
