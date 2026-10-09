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
	Role              string            `json:"role"`
	Operator          bool              `json:"operator"`
	MemberMode        bool              `json:"user_view"`
	UserType          *meUserTypeView   `json:"user_type"`
	UserViewDropped   map[string]string `json:"user_view_dropped"`
	UserViewPreselect string            `json:"user_view_preselect_type"`
	UserViewTypes     []meUserTypeView  `json:"user_view_types"`
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

// TestUserViewRealUserIsIgnoredBeforeTheTypeLookup (#997): a real user's
// POST /me/view answers the same no-op 200 whether the type it names exists
// or not, so it cannot probe which type ids exist.
func TestUserViewRealUserIsIgnoredBeforeTheTypeLookup(t *testing.T) {
	srv, _, _ := uvServer(t)
	user := uvSession(t, "sub-uv-user", oidc.RoleUser, types.UserTypeStandard, "")
	var bodies []string
	for _, id := range []string{utPM, "no-such-type"} {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", user, `{"view":"user","user_type":"`+id+`"}`)
		if w.Code != http.StatusOK || len(w.Result().Cookies()) != 0 {
			t.Fatalf("real user asking for %q = %d %s, cookies %v, want the no-op 200", id, w.Code, w.Body.String(), w.Result().Cookies())
		}
		bodies = append(bodies, w.Body.String())
	}
	if bodies[0] != bodies[1] {
		t.Errorf("a known and an unknown type answer differently:\n%s\n%s", bodies[0], bodies[1])
	}
	// The admin still gets the 400: the lookup is only skipped for a real user.
	admin := uvSession(t, uvAdminSub, oidc.RoleAdmin, types.UserTypeStandard, "")
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","user_type":"no-such-type"}`); w.Code != http.StatusBadRequest {
		t.Fatalf("admin asking for an unknown type = %d %s, want 400", w.Code, w.Body.String())
	}
}

// TestUserViewForcedExitWritesOneRow (#1020): the forced exit GET /me takes when the viewed type is
// gone writes auth.user_view.set once, and no row under the removed pre-0.8 name.
func TestUserViewForcedExitWritesOneRow(t *testing.T) {
	srv, _, h := uvServer(t)
	uvGetMe(t, srv, uvSession(t, uvAdminSub, oidc.RoleAdmin, types.UserTypeStandard, "contractor"))
	set := lastAuditEvent(t, h.audit.events, "auth.user_view.set")
	if !strings.Contains(string(set.Data), `"reason":"user_type_deleted"`) {
		t.Fatalf("forced exit datum = %s", set.Data)
	}
	if got := countAuditEvents(h.audit.events, "auth.member_mode"); got != 0 {
		t.Fatalf("auth.member_mode count = %d, want 0", got)
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

// TestMeUserViewPreselectType (#912): /me's user_view_preselect_type is the
// same resolution POST /me/view applies with no type named — the remembered
// choice, else the admin's own stamped type, else the built-in one — computed
// BEFORE the view is entered so the switch's dropdown has a first value to
// show. It answers "" once the view is already on and for a real user, since
// neither ever renders the picker.
func TestMeUserViewPreselectType(t *testing.T) {
	srv, st, _ := uvServer(t)

	// No remembered choice yet: the admin's own stamped type.
	admin := uvSession(t, uvAdminSub, oidc.RoleAdmin, utDev, "")
	me, _ := uvGetMe(t, srv, admin)
	if me.UserViewPreselect != utDev {
		t.Fatalf("preselect with no remembered choice = %q, want the stamped type %q", me.UserViewPreselect, utDev)
	}

	// Remember a different choice; a NEW session of the same principal, still
	// outside the view, preselects it over the stamped type.
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","user_type":"`+utPM+`"}`); w.Code != http.StatusOK {
		t.Fatalf("choose %q: %d %s", utPM, w.Code, w.Body.String())
	}
	fresh := uvSession(t, uvAdminSub, oidc.RoleAdmin, utDev, "")
	me, _ = uvGetMe(t, srv, fresh)
	if me.UserViewPreselect != utPM {
		t.Fatalf("preselect with a remembered choice = %q, want %q", me.UserViewPreselect, utPM)
	}

	// Already in the view: /me's own user_type field answers this, so the
	// preselect is blank.
	inView := uvSession(t, uvAdminSub, oidc.RoleAdmin, utDev, utPM)
	me, _ = uvGetMe(t, srv, inView)
	if me.UserViewPreselect != "" {
		t.Fatalf("preselect while already in the view = %q, want empty", me.UserViewPreselect)
	}

	// A real user never renders the picker either.
	st.userTypes = utKnown
	user := uvSession(t, "sub-uv-preselect-user", oidc.RoleUser, types.UserTypeStandard, "")
	me, _ = uvGetMe(t, srv, user)
	if me.UserViewPreselect != "" {
		t.Fatalf("preselect for a real user = %q, want empty", me.UserViewPreselect)
	}
}

// TestMeUserViewTypes (#912, H2): GET /user-types is securityOps and 403s a
// session CLAMPED to user by the view — this is what /me's user_view_types
// is for instead. An admin gets the org's types whether or not they have
// entered the view yet (their STAMPED role decides, never the clamped one);
// a real user — who has no stamped role above user to underlie any clamp —
// never gets the list, which is what keeps the org's type roster off a
// member's own /me.
func TestMeUserViewTypes(t *testing.T) {
	srv, _, _ := uvServer(t)

	// An admin outside the view: the picker's own pre-entry data source.
	// (uvServer's fake store answers ListUserTypes with the one built-in
	// type regardless of seeding — this test is about WHO gets the list, not
	// how many rows are in it; a real store's row count is store_user_types's
	// own concern.)
	admin := uvSession(t, uvAdminSub, oidc.RoleAdmin, types.UserTypeStandard, "")
	me, _ := uvGetMe(t, srv, admin)
	if len(me.UserViewTypes) != 1 || me.UserViewTypes[0].ID != types.UserTypeStandard {
		t.Fatalf("admin outside the view: user_view_types = %+v, want the built-in type", me.UserViewTypes)
	}

	// The SAME admin, now clamped INSIDE the view: GET /user-types itself
	// would 403 here (securityOps, clamped role); /me must still answer.
	inView := uvSession(t, uvAdminSub, oidc.RoleAdmin, utDev, utPM)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/user-types", inView, ""); w.Code != http.StatusForbidden {
		t.Fatalf("GET /user-types inside the view = %d, want 403 (the clamp this field exists to work around)", w.Code)
	}
	me, _ = uvGetMe(t, srv, inView)
	if len(me.UserViewTypes) != 1 {
		t.Fatalf("admin inside the view: user_view_types = %+v, want the built-in type", me.UserViewTypes)
	}

	// A security admin gets it too — the same tier GET /user-types itself
	// admits outside any view.
	secAdmin := uvSession(t, "sub-uv-secadmin", oidc.RoleSecurityAdmin, types.UserTypeStandard, "")
	me, _ = uvGetMe(t, srv, secAdmin)
	if len(me.UserViewTypes) != 1 {
		t.Fatalf("security admin: user_view_types = %+v, want the built-in type", me.UserViewTypes)
	}

	// A real member: never the org's type roster, on any request shape.
	user := uvSession(t, "sub-uv-types-user", oidc.RoleUser, types.UserTypeStandard, "")
	me, _ = uvGetMe(t, srv, user)
	if me.UserViewTypes != nil {
		t.Fatalf("real user: user_view_types = %+v, want nil — never expose the org's types to a member", me.UserViewTypes)
	}
}

// TestUserViewDroppedCarriesTheCachedName (M4): the drop notice must name the
// removed type, not just its id — but the type's row is gone by the time the
// drop fires, so the only way to answer with a name is one cached at the
// switch that entered it. This proves the cache survives the round trip:
// choose a real type (POST /me/view resolves and caches its name), delete
// it, trip the drop, and read the name back off /me.
func TestUserViewDroppedCarriesTheCachedName(t *testing.T) {
	srv, st, _ := uvServer(t)
	st.userTypes = []types.UserType{{ID: utPM, Name: "Portfolio manager"}}
	admin := uvSession(t, uvAdminSub, oidc.RoleAdmin, types.UserTypeStandard, "")

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","user_type":"`+utPM+`"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("choose %q: %d %s", utPM, w.Code, w.Body.String())
	}
	chosen, sess := reSigned(t, w)
	if sess.UserViewTypeName != "Portfolio manager" {
		t.Fatalf("cached name after choosing = %q, want %q", sess.UserViewTypeName, "Portfolio manager")
	}

	// The row is gone: the next request trips the drop.
	st.userTypes = nil
	w = doSSO(t, srv, http.MethodGet, "/api/v1/me/capabilities", chosen, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("request after the delete = %d %s, want 403", w.Code, w.Body.String())
	}
	dropped, sess := reSigned(t, w)
	if sess.UserViewDroppedName != "Portfolio manager" {
		t.Fatalf("cached name after the drop = %q, want %q", sess.UserViewDroppedName, "Portfolio manager")
	}

	me, _ := uvGetMe(t, srv, dropped)
	if me.UserViewDropped["user_type"] != utPM || me.UserViewDropped["user_type_name"] != "Portfolio manager" {
		t.Fatalf("/me user_view_dropped = %+v, want user_type %q and user_type_name %q", me.UserViewDropped, utPM, "Portfolio manager")
	}
}

// TestMeUserViewTypes_SecurityAdminInsideTheView (round-2 review P-C): the
// builder's own TestMeUserViewTypes checked this tier only OUTSIDE the view;
// a security_admin looking through a chosen type must get the roster too,
// for the identical reason an admin does.
func TestMeUserViewTypes_SecurityAdminInsideTheView(t *testing.T) {
	srv, _, _ := uvServer(t)
	inView := uvSession(t, "sub-uv-secadmin-inview", oidc.RoleSecurityAdmin, types.UserTypeStandard, utDev)
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/user-types", inView, ""); w.Code != http.StatusForbidden {
		t.Fatalf("GET /user-types inside the view (security_admin) = %d, want 403", w.Code)
	}
	me, _ := uvGetMe(t, srv, inView)
	if len(me.UserViewTypes) != 1 {
		t.Fatalf("security_admin inside the view: user_view_types = %+v, want the built-in type", me.UserViewTypes)
	}
}

// TestMeUserViewTypes_MemberModeCookieOnAnExistingTypeGetsTheRoster (round-3
// review, F1): meUserViewTypes's own gate does NOT distinguish "a real admin
// looking through a type" from "a cookie that hand-builds the same shape on
// a stamped user" — both have MemberMode true and a type that exists, so
// both get the roster, by design (round 2's own remediation (b): MemberMode
// implies the stamped operator tier BY CONSTRUCTION, so the gate does not
// need to re-check it). What actually keeps a real member from ever reaching
// this state is NOT this function and is NOT tested here — it is:
//   - SetUserView (the mode's ONE writer) refusing to turn it on for a
//     stamped user, pinned by oidc's own
//     TestMemberMode_RealMemberTurningItOnWritesNoCookie and by this
//     package's TestUserViewSwitchValidatesAndRemembersTheType ("a real user
//     is already in the user view: nothing is written or remembered");
//   - the cookie's HMAC signature, which makes this exact shape unforgeable
//     without the server's key, pinned by
//     TestMeUserViewTypes_ForgedCookieIs401 above.
// (A previous version of this test named a type that does not exist, so
// userViewGate dropped the view before /me ever ran — it passed for a reason
// that had nothing to do with meUserViewTypes, and its own comment wrongly
// credited that drop as a "defence in depth" for this field. There is no
// such defence here; the two tests named above are the real ones.)
func TestMeUserViewTypes_MemberModeCookieOnAnExistingTypeGetsTheRoster(t *testing.T) {
	srv, _, _ := uvServer(t)
	handBuilt := uvSession(t, "sub-uv-handbuilt-member", oidc.RoleUser, types.UserTypeStandard, utPM)
	// GET /me answers straight from this cookie and sets no new one — the
	// named type exists, so userViewGate has nothing to drop, and this
	// request itself changes no state.
	me, w := uvGetMe(t, srv, handBuilt)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me = %d %s", w.Code, w.Body.String())
	}
	if len(me.UserViewTypes) != 1 {
		t.Fatalf("MemberMode on an existing type: user_view_types = %+v, want the built-in type (the gate reads MemberMode, not who is stamped)", me.UserViewTypes)
	}
	if !me.MemberMode {
		t.Errorf("me.user_view = %v, want true — this state is never dropped", me.MemberMode)
	}
}

// TestMeUserViewTypes_ForgedCookieIs401 (round-2 review): a session cookie
// whose signature no longer verifies must never reach any handler logic —
// GET /me answers 401, not a resolved (or even a nil) user_view_types.
func TestMeUserViewTypes_ForgedCookieIs401(t *testing.T) {
	srv, _, _ := uvServer(t)
	good := uvSession(t, "sub-uv-forged", oidc.RoleAdmin, types.UserTypeStandard, "")
	forged := *good
	// Flips the base64 payload's first character to a different one from the
	// SAME alphabet (RawURLEncoding never emits '_' first, so this is always
	// a change) — corrupts the signed payload while staying a byte the
	// cookie wire format accepts, unlike an arbitrary XOR'd byte.
	if forged.Value[0] == '_' {
		forged.Value = "-" + forged.Value[1:]
	} else {
		forged.Value = "_" + forged.Value[1:]
	}
	if w := doSSO(t, srv, http.MethodGet, "/api/v1/me", &forged, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("GET /me with a forged cookie = %d, want 401", w.Code)
	}
}

// TestMeUserViewTypes_TokenLaneMemberGetsNil (round-2 review): the wdn_ token
// lane publishes identity through the SAME withHumanIdentity the SSO branch
// uses (apitokens.go), never through oidc.contextWithPrincipal — so
// oidc.MemberModeFromContext is always false there (the user view is a
// cookie-only concept; SetUserView needs one to write to). A member's own
// token must still never see the org's type roster.
func TestMeUserViewTypes_TokenLaneMemberGetsNil(t *testing.T) {
	srv, _, _ := uvServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	ctx := withHumanIdentity(req.Context(), "sub-uv-token-member", "member@corp.example", oidc.RoleUser, types.UserTypeStandard, nil, false)
	if got := srv.meUserViewTypes(req.WithContext(ctx)); got != nil {
		t.Fatalf("token-lane member: user_view_types = %+v, want nil", got)
	}
}

// TestMeUserViewTypes_BareAdminTokenGetsNil: the bare admin token (env-var
// bootstrap credential, no per-human identity) is exempt in isSecurityOperator
// itself ("no verified OIDC human" arm, http.go), which would otherwise make
// it look like a security operator here too. The explicit human check is
// what keeps this lane at nil — it already reaches GET /user-types directly
// (isOperator's own exemption), so /me owes it nothing extra.
func TestMeUserViewTypes_BareAdminTokenGetsNil(t *testing.T) {
	srv, _, _ := uvServer(t)
	w := do(t, srv, http.MethodGet, "/api/v1/me", adminToken, "")
	var got uvMe
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil {
		t.Fatalf("GET /me with the bare admin token = %d %s", w.Code, w.Body.String())
	}
	if got.UserViewTypes != nil {
		t.Fatalf("bare admin token: user_view_types = %+v, want nil", got.UserViewTypes)
	}
}
