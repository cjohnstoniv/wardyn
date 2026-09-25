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

// userViewRequest is POST /me/view's body.
type userViewRequest struct {
	View string `json:"view"` // "user" or "admin"
	// UserType is the type to view as; empty means the previous choice, then
	// the admin's own type, then the built-in type.
	UserType     string `json:"user_type"`
	NoCredential bool   `json:"no_credential"`
}

// handleSetUserView is POST /api/v1/me/view. It shares handleSetMemberMode's
// guards and posture rules; what it adds is the type the view looks through,
// validated here and again on every request (userViewGate).
func (s *Server) handleSetUserView(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sub := oidcHumanFromContext(ctx)
	if sub == "" || s.cfg.OIDC == nil {
		writeError(w, http.StatusBadRequest, memberModeNoHumanRefusal)
		return
	}
	var req userViewRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	on := req.View == "user"
	if !on && req.View != "admin" {
		writeError(w, http.StatusBadRequest, `The view must be "user" or "admin".`)
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
	preview := on && req.NoCredential && s.memberPreviewApplies(ctx, r)
	realRole, err := s.cfg.OIDC.SetUserView(w, r, on, typeID, preview)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "no session to change: sign in again")
		return
	}
	// A real user asking for the user view is already in it: SetUserView wrote
	// nothing, so nothing is remembered, audited as entered, or reported.
	entered := on && realRole != oidc.RoleUser
	if entered {
		s.rememberUserViewType(ctx, sub, typeID)
	}
	datum := map[string]any{"enabled": on, "real_role": realRole}
	if entered {
		datum["user_type"] = typeID
		if preview {
			datum["no_credential"] = true
		}
	}
	s.recordAudit(ctx, s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"auth.user_view", "/api/v1/me/view", "success", mustJSON(datum)))
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
	switch r.URL.Path {
	case "/api/v1/me/view", "/api/v1/me/member-mode":
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
