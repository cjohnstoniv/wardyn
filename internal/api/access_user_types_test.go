// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The People page over user types (UT-2b): a role mapping names a type, the
// write boundary refuses a type that does not exist, GET /access and the
// preview name the type, and /me reports the session's type.

var accessOrgTypes = []types.UserType{
	{ID: "portfolio-manager", Name: "Portfolio manager", Priority: 10},
	{ID: "analyst", Name: "Analyst", Priority: 10},
}

func TestAccess_UpsertNamesAUserType(t *testing.T) {
	cases := []struct {
		name, body string
		wantCode   int
		wantType   string
		wantErr    string
	}{
		{"a type id", `{"value":"pm-group","role":"user","user_type":"portfolio-manager"}`, http.StatusCreated, "portfolio-manager", ""},
		{"no type is the built-in one", `{"value":"eng-team","role":"user"}`, http.StatusCreated, types.UserTypeStandard, ""},
		{"the built-in type by name", `{"value":"eng-team","role":"user","user_type":"standard"}`, http.StatusCreated, types.UserTypeStandard, ""},
		{"a type that does not exist", `{"value":"x","role":"user","user_type":"contractor"}`, http.StatusBadRequest, "",
			`The user type "contractor" doesn't exist. Create it under User types first.`},
		{"a type on an admin row", `{"value":"x","role":"admin","user_type":"portfolio-manager"}`, http.StatusBadRequest, "",
			"Only the user role takes a user type."},
		{"a type on a security admin row", `{"value":"x","role":"security_admin","user_type":"standard"}`, http.StatusBadRequest, "",
			"Only the user role takes a user type."},
		{"a reserved word as the type", `{"value":"x","role":"user","user_type":"admin"}`, http.StatusBadRequest, "",
			`The user type "admin" isn't a valid id.`},
		{"a malformed type id", `{"value":"x","role":"user","user_type":"Portfolio Manager"}`, http.StatusBadRequest, "",
			`The user type "Portfolio Manager" isn't a valid id.`},
		{"a type id as the role", `{"value":"x","role":"portfolio-manager"}`, http.StatusBadRequest, "",
			`The role "portfolio-manager" isn't valid (want "admin", "security_admin" or "user").`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, "", nil, nil)
			st := &roleMapStore{userTypes: accessOrgTypes}
			srv := accessServer(t, auth, st)
			w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, c.body)
			if w.Code != c.wantCode {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, c.wantCode, w.Body.String())
			}
			if c.wantCode != http.StatusCreated {
				if len(st.rows) != 0 {
					t.Errorf("a refused write stored %+v", st.rows)
				}
				if c.wantErr != "" {
					var body errorBody
					_ = json.Unmarshal(w.Body.Bytes(), &body)
					if body.Error != c.wantErr {
						t.Errorf("error = %q, want %q", body.Error, c.wantErr)
					}
				}
				return
			}
			if len(st.rows) != 1 || st.rows[0].Role != oidc.RoleUser || st.rows[0].UserType != c.wantType {
				t.Fatalf("stored = %+v, want one user row on %q", st.rows, c.wantType)
			}
		})
	}
}

// TestAccess_UpsertTypeDeletedMidWrite: the foreign key refusing the row (the
// type was removed after the handler read the list) is the same 400, not a 500.
func TestAccess_UpsertTypeDeletedMidWrite(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"chart-admin": oidc.RoleAdmin}, "", nil, nil)
	st := &fkRoleMapStore{roleMapStore: &roleMapStore{userTypes: accessOrgTypes}}
	cfg := baseTestConfig(newHarness(t), st)
	cfg.OIDC = auth
	srv := New(cfg)
	w := do(t, srv, http.MethodPost, "/api/v1/access/mappings", adminToken, `{"value":"pm-group","role":"user","user_type":"analyst"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), `The user type \"analyst\" doesn't exist.`) {
		t.Fatalf("status = %d body=%s, want the 400 naming the type", w.Code, w.Body.String())
	}
}

type fkRoleMapStore struct{ *roleMapStore }

func (s *fkRoleMapStore) UpsertRoleMapping(context.Context, types.RoleMapping) (types.RoleMapping, error) {
	return types.RoleMapping{}, store.ErrNotFound
}

// TestAccess_GetNamesTheTypeOnEachRow: a chart value that is a type id reads
// as the user tier on that type, a console user row with no type reads as the
// built-in one, and the types themselves ride along for their names.
func TestAccess_GetNamesTheTypeOnEachRow(t *testing.T) {
	auth := newAccessAuth(t, map[string]string{"wardyn.admin": oidc.RoleAdmin, "pm-group": "portfolio-manager", "wardyn.member": oidc.RoleUser}, "analyst", nil, nil)
	st := &roleMapStore{userTypes: accessOrgTypes, rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "old-row", Role: oidc.RoleUser},
		{ID: uuid.New(), Value: "quant-group", Role: oidc.RoleUser, UserType: "analyst"},
	}}
	srv := accessServer(t, auth, st)
	w := do(t, srv, http.MethodGet, "/api/v1/access", adminToken, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
	}
	var resp accessResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	got := map[string][2]string{}
	for _, m := range resp.Mappings {
		got[m.Value] = [2]string{m.Role, m.UserType}
	}
	want := map[string][2]string{
		"wardyn.admin":  {oidc.RoleAdmin, ""},
		"pm-group":      {oidc.RoleUser, "portfolio-manager"},
		"wardyn.member": {oidc.RoleUser, types.UserTypeStandard},
		"old-row":       {oidc.RoleUser, types.UserTypeStandard},
		"quant-group":   {oidc.RoleUser, "analyst"},
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("row %q = %v, want %v", k, got[k], v)
		}
	}
	ids := []string{}
	for _, ut := range resp.UserTypes {
		ids = append(ids, ut.ID)
	}
	if !slices.Equal(ids, []string{types.UserTypeStandard, "portfolio-manager", "analyst"}) {
		t.Errorf("user_types = %v, want the built-in type and both custom ones", ids)
	}
	if resp.DefaultRole != "analyst" || resp.Posture.After != "analyst" {
		t.Errorf("default_role = %q, posture.after = %q; want the type id", resp.DefaultRole, resp.Posture.After)
	}
}

// TestAccessRolePosture_DefaultTypeIsAChange: arm 1's users are on the
// built-in type, so a default naming a custom type moves them; "standard"
// spelled out is the same as "user".
func TestAccessRolePosture_DefaultTypeIsAChange(t *testing.T) {
	emails := []string{"ops@corp.example"}
	for _, c := range []struct {
		def, after string
		changes    bool
	}{
		{"analyst", "analyst", true},
		{types.UserTypeStandard, oidc.RoleUser, false},
		{oidc.RoleUser, oidc.RoleUser, false},
	} {
		before, after, changes := accessRolePosture(newAccessAuth(t, nil, c.def, emails, nil))
		if before != oidc.RoleUser || after != c.after || changes != c.changes {
			t.Errorf("default %q: posture = (%q, %q, %v), want (user, %q, %v)", c.def, before, after, changes, c.after, c.changes)
		}
	}
}

// TestAccess_PreviewNamesTheTypeAndTheTie: the preview carries the type a
// sign-in would get, and for a tie the denial and both type ids.
func TestAccess_PreviewNamesTheTypeAndTheTie(t *testing.T) {
	st := &roleMapStore{userTypes: accessOrgTypes, rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "pm-group", Role: oidc.RoleUser, UserType: "portfolio-manager"},
		{ID: uuid.New(), Value: "quant-group", Role: oidc.RoleUser, UserType: "analyst"},
		{ID: uuid.New(), Value: "wardyn.member", Role: oidc.RoleUser, UserType: types.UserTypeStandard},
	}}
	auth := newAccessAuth(t, nil, "", nil, st)
	srv := accessServer(t, auth, st)

	preview := func(body string) accessPreviewResponse {
		t.Helper()
		w := do(t, srv, http.MethodPost, "/api/v1/access/preview", adminToken, body)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d; body=%s", w.Code, w.Body.String())
		}
		var resp accessPreviewResponse
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}
	if r := preview(`{"groups":["pm-group","wardyn.member"]}`); !r.OK || r.Role != oidc.RoleUser || r.UserType != "portfolio-manager" || len(r.Matched) != 2 {
		t.Errorf("preview = %+v, want user on portfolio-manager with both matches", r)
	}
	r := preview(`{"groups":["pm-group","quant-group"]}`)
	if r.OK || r.Denial != oidc.DenialUserTypeAmbiguous || !slices.Equal(r.Tied, []string{"analyst", "portfolio-manager"}) {
		t.Errorf("tied preview = %+v, want the ambiguity with both ids", r)
	}
	if r := preview(`{"groups":["nobody"]}`); r.OK || r.Denial != oidc.DenialNoRole {
		t.Errorf("no-match preview = %+v, want denial no_role", r)
	}
}

// TestAccess_LockoutGuardAndAnAdminsTypeTie: a type tie never refuses an admin
// sign-in, so it trips neither side of the lockout guard: a write that ties
// the acting admin's types is saved, and so is a write made while they already
// tie. A write that takes the admin tier away is still the lockout.
func TestAccess_LockoutGuardAndAnAdminsTypeTie(t *testing.T) {
	auth := newAccessAuth(t, nil, "", nil, nil)
	st := &roleMapStore{userTypes: accessOrgTypes, rows: []types.RoleMapping{
		{ID: uuid.New(), Value: "admins", Role: oidc.RoleAdmin},
		{ID: uuid.New(), Value: "pm-group", Role: oidc.RoleUser, UserType: "portfolio-manager"},
	}}
	srv := accessServer(t, auth, st)
	admin := accessSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin, []string{"admins", "pm-group", "quant-group"})

	for _, body := range []string{
		`{"value":"quant-group","role":"user","user_type":"analyst"}`, // the write that ties them
		`{"value":"eng-team","role":"user"}`,                          // a write while they tie
	} {
		if w := doSSO(t, srv, http.MethodPost, "/api/v1/access/mappings", admin, body); w.Code != http.StatusCreated {
			t.Fatalf("POST %s = %d body=%s, want 201", body, w.Code, w.Body.String())
		}
	}
	w := doSSO(t, srv, http.MethodPost, "/api/v1/access/mappings", admin, `{"value":"admins","role":"user"}`)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "remove your own admin access") {
		t.Fatalf("status = %d body=%s, want the lockout refusal", w.Code, w.Body.String())
	}
	if len(st.rows) != 4 {
		t.Errorf("rows = %+v, want the two saved writes and not the refused one", st.rows)
	}
}

// TestMe_ReportsTheSessionUserType: /me names the session's type with its
// display name; a caller with no SSO session has none.
func TestMe_ReportsTheSessionUserType(t *testing.T) {
	cfg := baseTestConfig(newHarness(t), meTypeStore{})
	cfg.OIDC = newAccessAuth(t, nil, "", nil, nil)
	srv := New(cfg)

	read := func(w *httptest.ResponseRecorder) map[string]any {
		t.Helper()
		if w.Code != http.StatusOK {
			t.Fatalf("GET /me = %d; body=%s", w.Code, w.Body.String())
		}
		var body map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return body
	}
	body := read(doSSO(t, srv, http.MethodGet, "/api/v1/me", accessSessionOfType(t, "sub-pm", "pat@corp.example", oidc.RoleUser, "portfolio-manager", []string{}), ""))
	if ut, _ := body["user_type"].(map[string]any); ut["id"] != "portfolio-manager" || ut["name"] != "Portfolio manager" {
		t.Errorf("user_type = %v, want portfolio-manager / Portfolio manager", body["user_type"])
	}
	body = read(doSSO(t, srv, http.MethodGet, "/api/v1/me", accessSessionOfType(t, "sub-gone", "g@corp.example", oidc.RoleUser, "contractor", []string{}), ""))
	if ut, _ := body["user_type"].(map[string]any); ut["id"] != "contractor" || ut["name"] != "" {
		t.Errorf("user_type for a removed type = %v, want the id with no name", body["user_type"])
	}
	body = read(do(t, srv, http.MethodGet, "/api/v1/me", adminToken, ""))
	if v, ok := body["user_type"]; !ok || v != nil {
		t.Errorf("admin token user_type = %v (present %v), want null", v, ok)
	}
}

// meTypeStore answers /me's reads for a session on a custom type.
type meTypeStore struct{ rbacStore }

func (meTypeStore) GetUserType(_ context.Context, id string) (types.UserType, error) {
	for _, t := range accessOrgTypes {
		if t.ID == id {
			return t, nil
		}
	}
	return types.UserType{}, store.ErrNotFound
}

// TestAuditSignInDenied: the two user-type refusals are auth.failed rows from
// the callback's own boundary; any other reason writes nothing.
func TestAuditSignInDenied(t *testing.T) {
	h := newHarness(t)
	srv := New(baseTestConfig(h, &roleMapStore{}))
	for _, reason := range []string{oidc.DenialUserTypeAmbiguous, oidc.DenialUserTypeUnknown, oidc.DenialNoRole} {
		srv.auditSignInDenied(httptest.NewRequest(http.MethodGet, "/auth/callback?reason="+reason, nil), reason)
	}
	var got []string
	for _, ev := range h.audit.snapshot() {
		if ev.Action != "auth.failed" {
			continue
		}
		if ev.Actor != oidcCallbackActor {
			t.Errorf("actor = %q, want %q", ev.Actor, oidcCallbackActor)
		}
		var data struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(ev.Data, &data)
		got = append(got, data.Reason)
	}
	if !slices.Equal(got, []string{authFailedUserTypeAmbiguous, authFailedUserTypeUnknown}) {
		t.Errorf("auth.failed reasons = %v, want the two user-type refusals only", got)
	}
	if authFailedUserTypeAmbiguous != oidc.DenialUserTypeAmbiguous || authFailedUserTypeUnknown != oidc.DenialUserTypeUnknown {
		t.Error("the audit reasons drifted from the sign-in denial codes")
	}
	if !oidc.UserTypeIDReserved(accessDeniedRole) {
		t.Errorf("%q is the People page's no-role word but not a reserved type id", accessDeniedRole)
	}
}
