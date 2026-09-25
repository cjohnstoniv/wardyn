// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The user view's chosen type (user-types design §2.7): POST /me/view picks
// it, contextWithPrincipal publishes it, and a type deleted mid-session
// refuses the next request instead of answering it as the admin.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const uvAdminSub = "sub-uv-admin"

// uvStore is permStore plus the principal_prefs table.
type uvStore struct {
	*permStore
	prefs map[string]json.RawMessage
}

func (s *uvStore) GetPrincipalPref(_ context.Context, principal, key string) (json.RawMessage, error) {
	v, ok := s.prefs[principal+"|"+key]
	if !ok {
		return nil, store.ErrNotFound
	}
	return v, nil
}

func (s *uvStore) PutPrincipalPref(_ context.Context, principal, key string, value json.RawMessage) error {
	s.prefs[principal+"|"+key] = value
	return nil
}

func uvServer(t *testing.T) (*Server, *uvStore, *harness) {
	t.Helper()
	h := newHarness(t)
	st := &uvStore{permStore: &permStore{capStore: &capStore{userTypes: utKnown}}, prefs: map[string]json.RawMessage{}}
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	return New(cfg), st, h
}

// uvSession hand-signs a session (the zero-value Authenticator's key) for sub,
// stamped with role and stampedType, in the user view looking through
// viewType when viewType is not "".
func uvSession(t *testing.T, sub, role, stampedType, viewType string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: sub + "@corp.example", Role: role, UserType: stampedType,
		MemberMode: viewType != "", UserViewType: viewType, Expiry: time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, nil)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// reSigned returns the session cookie a response set, decoded, or fails.
func reSigned(t *testing.T, w *httptest.ResponseRecorder) (*http.Cookie, oidc.Session) {
	t.Helper()
	for _, c := range w.Result().Cookies() {
		if c.Name != "wardyn_session" {
			continue
		}
		raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(c.Value, ".", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		var sess oidc.Session
		if err := json.Unmarshal(raw, &sess); err != nil {
			t.Fatal(err)
		}
		return c, sess
	}
	t.Fatalf("response set no session cookie (%d %s)", w.Code, w.Body.String())
	return nil, oidc.Session{}
}

type uvMe struct {
	Role            string            `json:"role"`
	Operator        bool              `json:"operator"`
	MemberMode      bool              `json:"user_view"`
	UserType        *meUserTypeView   `json:"user_type"`
	UserViewDropped map[string]string `json:"user_view_dropped"`
}

func uvGetMe(t *testing.T, srv *Server, c *http.Cookie) (uvMe, *httptest.ResponseRecorder) {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/me", c, "")
	var me uvMe
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &me) != nil {
		t.Fatalf("GET /me = %d %s", w.Code, w.Body.String())
	}
	return me, w
}

// TestUserViewResolvesAsTheChosenType: in the user view the chosen type's
// rows bind the admin — not the type stamped at sign-in — and the tier stays
// clamped, so no admin route opens whatever the type.
func TestUserViewResolvesAsTheChosenType(t *testing.T) {
	srv, st, _ := uvServer(t)
	st.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utPM, capAgent, "claude-code", types.CapabilityDeny)}
	view := uvSession(t, uvAdminSub, oidc.RoleAdmin, types.UserTypeStandard, utPM)

	me, _ := uvGetMe(t, srv, view)
	if me.Role != oidc.RoleUser || me.Operator || me.UserType == nil || me.UserType.ID != utPM || me.UserViewDropped != nil {
		t.Fatalf("/me in the view = %+v, want role user, not operator, user_type %q, nothing dropped", me, utPM)
	}

	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/capabilities", view, "")
	var caps meCapabilitiesResponse
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &caps) != nil {
		t.Fatalf("GET /me/capabilities = %d %s", w.Code, w.Body.String())
	}
	if len(caps.Grants) != 1 || caps.Grants[0].Subject != utPM {
		t.Fatalf("grants = %+v, want the chosen type's deny", caps.Grants)
	}

	for _, path := range []string{"/api/v1/access", "/api/v1/user-types", "/api/v1/permissions"} {
		method := http.MethodGet
		if path == "/api/v1/user-types" {
			method = http.MethodPost
		}
		if w := doSSO(t, srv, method, path, view, `{}`); w.Code != http.StatusForbidden {
			t.Errorf("%s %s in the view = %d, want 403: %s", method, path, w.Code, w.Body.String())
		}
	}
}

// TestUserViewDeletedTypeRefusesTheNextRequest is the fail-closed pin: once
// the viewed type is gone, the next request is refused (a launch with the S1
// 409), never answered as the admin, and the view is turned off on the
// cookie. Only GET /me drops back to the admin's real tier, and it says why.
func TestUserViewDeletedTypeRefusesTheNextRequest(t *testing.T) {
	srv, _, h := uvServer(t)
	const gone = "contractor"
	view := uvSession(t, uvAdminSub, oidc.RoleAdmin, types.UserTypeStandard, gone)

	w := doSSO(t, srv, http.MethodGet, "/api/v1/me/capabilities", view, "")
	var body errorBody
	if w.Code != http.StatusForbidden || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Reason != "user_view_type_deleted" {
		t.Fatalf("request after the delete = %d %s, want 403 user_view_type_deleted", w.Code, w.Body.String())
	}
	if !strings.HasPrefix(body.Error, "The contractor user type was removed") {
		t.Errorf("sentence = %q", body.Error)
	}
	after, sess := reSigned(t, w)
	if sess.MemberMode || sess.UserViewType != "" || sess.UserViewDropped != gone || sess.Role != oidc.RoleAdmin {
		t.Fatalf("re-signed session = %+v, want the view off, %q dropped, the stamped tier kept", sess, gone)
	}
	ev := lastAuditEvent(t, h.audit.events, "authz.denied")
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil || data["reason"] != "user_view_type_deleted" || data["user_type"] != gone || ev.Actor != uvAdminSub {
		t.Fatalf("authz.denied = %s by %q, want reason user_view_type_deleted, user_type %q, the admin's sub", ev.Data, ev.Actor, gone)
	}

	// The launch doors answer the S1 409 and never reach the handler: the
	// store behind them would panic on a run write. The status is asserted
	// against the registry, not the 409 literal, so a Lookup(ReasonAdminView)
	// that drifted from EffectConflict would fail here rather than agree with
	// a hand-picked constant.
	adminViewRef, ok := authz.Lookup(authz.ReasonAdminView)
	if !ok || adminViewRef.Effect != authz.EffectConflict || adminViewRef.Effect.Status() != http.StatusConflict {
		t.Fatalf("authz.ReasonAdminView registry row = %+v, ok=%v, want EffectConflict/409", adminViewRef, ok)
	}
	for _, path := range []string{"/api/v1/runs", "/api/v1/runs/preflight"} {
		w := doSSO(t, srv, http.MethodPost, path, view, `{"agent":"claude-code","task":"t"}`)
		if w.Code != adminViewRef.Effect.Status() || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Reason != "admin_view" {
			t.Fatalf("POST %s after the delete = %d %s, want %d admin_view", path, w.Code, w.Body.String(), adminViewRef.Effect.Status())
		}
		// The cause row still records user_view_type_deleted, with what the
		// response actually answered riding beside it.
		ev := lastAuditEvent(t, h.audit.events, "authz.denied")
		var data map[string]any
		if err := json.Unmarshal(ev.Data, &data); err != nil || data["reason"] != "user_view_type_deleted" || data["answered"] != "admin_view" {
			t.Fatalf("POST %s authz.denied = %s, want reason user_view_type_deleted, answered admin_view", path, ev.Data)
		}
	}

	// An admin route is refused in the same request, not opened: its tier
	// was read as user.
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions", view, ""); w.Code != http.StatusForbidden {
		t.Fatalf("admin route after the delete = %d %s, want 403", w.Code, w.Body.String())
	}

	// GET /me with the stale cookie drops back and answers the real tier.
	me, w := uvGetMe(t, srv, view)
	if me.Role != oidc.RoleAdmin || !me.Operator || me.MemberMode || me.UserViewDropped["user_type"] != gone || me.UserViewDropped["reason"] != "deleted" {
		t.Fatalf("/me after the delete = %+v, want the admin tier with user_view_dropped %q", me, gone)
	}
	if _, sess := reSigned(t, w); sess.MemberMode || sess.UserViewDropped != gone {
		t.Fatalf("/me re-signed %+v, want the view off with %q dropped", sess, gone)
	}

	// The refused request's cookie is already in the Admin view, and keeps
	// the notice until the next switch.
	if me, _ := uvGetMe(t, srv, after); me.Role != oidc.RoleAdmin || me.UserViewDropped["user_type"] != gone {
		t.Fatalf("/me with the re-signed cookie = %+v", me)
	}
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", after, `{"view":"user","user_type":"`+utDev+`"}`)
	next, sess := reSigned(t, w)
	if sess.UserViewDropped != "" || sess.UserViewType != utDev {
		t.Fatalf("switch after the drop = %+v", sess)
	}
	if me, _ := uvGetMe(t, srv, next); me.UserViewDropped != nil || me.UserType.ID != utDev {
		t.Fatalf("/me after choosing another type = %+v", me)
	}
}

// TestUserViewSwitchValidatesAndRemembersTheType: POST /me/view refuses an
// unknown type, remembers the choice per principal (on any session of that
// principal), and falls back to the admin's own type, then the built-in one.
func TestUserViewSwitchValidatesAndRemembersTheType(t *testing.T) {
	srv, st, h := uvServer(t)
	admin := uvSession(t, uvAdminSub, oidc.RoleAdmin, utDev, "")

	for _, body := range []string{`{"view":"user","user_type":"nope"}`, `{"view":"sideways"}`} {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, body); w.Code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d %s, want 400", body, w.Code, w.Body.String())
		}
	}

	// No choice yet: the admin's own type.
	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user"}`)
	if _, sess := reSigned(t, w); !sess.MemberMode || sess.UserViewType != utDev || sess.UserType != utDev {
		t.Fatalf("default = %+v, want the view on as %q", sess, utDev)
	}

	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","user_type":"`+utPM+`"}`)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"user_type":"`+utPM+`"`) {
		t.Fatalf("choose %q = %d %s", utPM, w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.events, "auth.user_view.set")
	if !strings.Contains(string(ev.Data), `"user_type":"`+utPM+`"`) || ev.Actor != uvAdminSub {
		t.Fatalf("auth.user_view.set = %s by %q", ev.Data, ev.Actor)
	}

	// Another session of the same principal preselects the choice.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", uvSession(t, uvAdminSub, oidc.RoleAdmin, utDev, ""), `{"view":"user"}`)
	if _, sess := reSigned(t, w); sess.UserViewType != utPM {
		t.Fatalf("other session = %+v, want the remembered %q", sess, utPM)
	}
	// Another principal does not.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", uvSession(t, "sub-uv-other", oidc.RoleAdmin, types.UserTypeStandard, ""), `{"view":"user"}`)
	if _, sess := reSigned(t, w); sess.UserViewType != types.UserTypeStandard {
		t.Fatalf("other principal = %+v, want the built-in type", sess)
	}

	// A remembered type that was deleted, and an own type that was too: the
	// built-in type.
	st.userTypes = nil
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user"}`)
	if _, sess := reSigned(t, w); sess.UserViewType != types.UserTypeStandard {
		t.Fatalf("both gone = %+v, want the built-in type", sess)
	}

	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"admin"}`)
	if _, sess := reSigned(t, w); sess.MemberMode || sess.UserViewType != "" {
		t.Fatalf("exit = %+v, want the view off", sess)
	}

	// A real user is already in the user view: nothing is written or remembered.
	st.userTypes = utKnown
	user := uvSession(t, "sub-uv-user", oidc.RoleUser, types.UserTypeStandard, "")
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", user, `{"view":"user","user_type":"`+utPM+`"}`)
	if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 || st.prefs["sub-uv-user|"+userViewTypePref] != nil {
		t.Fatalf("real user = %d %s, cookies %v, want a no-op", w.Code, w.Body.String(), w.Result().Cookies())
	}
}

// TestRunCreateCarriesTheViewedType: the type a run is frozen with, and the
// run.create datum, follow the viewed type.
func TestRunCreateCarriesTheViewedType(t *testing.T) {
	ctx := withHumanIdentity(context.Background(), uvAdminSub, "", oidc.RoleUser, utPM, nil, false)
	if got := runCreatorUserType(ctx); got != utPM {
		t.Fatalf("runCreatorUserType = %q, want %q", got, utPM)
	}
	if got := runCreatorUserType(withHumanIdentity(context.Background(), uvAdminSub, "", oidc.RoleUser, "", nil, false)); got != types.UserTypeStandard {
		t.Fatalf("an unstamped human = %q, want the built-in type", got)
	}
	if got := runCreatorUserType(context.Background()); got != "" {
		t.Fatalf("no human = %q, want empty", got)
	}
	if d := withRunUserType(ctx, utPM, map[string]any{}); d["user_type"] != utPM || d["user_view"] != nil {
		t.Fatalf("outside the view = %v", d)
	}
}
