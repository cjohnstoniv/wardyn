// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestAdminViewLaunchRefused is S1 (M-8): an SSO browser session in the Admin
// view neither starts nor previews a run. The User view, a user, the admin
// token and a wdn_ token all still launch, and each door answers what it
// answers for them.
func TestAdminViewLaunchRefused(t *testing.T) {
	const body = `{"agent":"claude-code","task":"t"}`
	const adminSub = "sub-admin-view"
	for _, door := range []struct {
		path string
		ok   int
	}{
		{"/api/v1/runs", http.StatusCreated},
		{"/api/v1/runs/preflight", http.StatusOK},
	} {
		t.Run(door.path, func(t *testing.T) {
			for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin} {
				t.Run(role+" in the Admin view is refused", func(t *testing.T) {
					srv, st, _ := govEscapeFixture(t, &capStore{})
					w := doSSO(t, srv, http.MethodPost, door.path,
						memberModeSSOSession(t, adminSub, "av@corp.example", role, false), body)
					assertAdminViewRefusal(t, w)
					if len(st.runs) != 0 {
						t.Errorf("runs created = %d, want 0", len(st.runs))
					}
				})
			}

			// The cookie lane wins when a request carries both, so a bearer
			// beside an Admin-view session does not open the door.
			t.Run("an Admin-view session with a wdn_ bearer beside it is refused", func(t *testing.T) {
				srv, st, _ := govEscapeFixture(t, &capStore{})
				giveAdminToken(st, adminSub)
				r := httptest.NewRequest(http.MethodPost, door.path, strings.NewReader(body))
				r.AddCookie(memberModeSSOSession(t, adminSub, "av@corp.example", oidc.RoleAdmin, false))
				r.Header.Set("Authorization", "Bearer "+st.tokenRaw)
				w := httptest.NewRecorder()
				panicFails(t, srv.Handler()).ServeHTTP(w, r)
				assertAdminViewRefusal(t, w)
			})

			for _, tc := range []struct {
				name string
				send func(t *testing.T, srv *Server, st *govEscapeStore) *httptest.ResponseRecorder
			}{
				{"the same admin in the User view", func(t *testing.T, srv *Server, _ *govEscapeStore) *httptest.ResponseRecorder {
					return doSSO(t, srv, http.MethodPost, door.path,
						memberModeSSOSession(t, adminSub, "av@corp.example", oidc.RoleAdmin, true), body)
				}},
				{"a user", func(t *testing.T, srv *Server, _ *govEscapeStore) *httptest.ResponseRecorder {
					return doSSO(t, srv, http.MethodPost, door.path, govSession(t, govMemberSub, []string{"eng"}, false), body)
				}},
				{"the admin token", func(t *testing.T, srv *Server, _ *govEscapeStore) *httptest.ResponseRecorder {
					return do(t, srv, http.MethodPost, door.path, adminToken, body)
				}},
				{"an admin's wdn_ token", func(t *testing.T, srv *Server, st *govEscapeStore) *httptest.ResponseRecorder {
					giveAdminToken(st, adminSub)
					return do(t, srv, http.MethodPost, door.path, st.tokenRaw, body)
				}},
			} {
				t.Run(tc.name+" is allowed", func(t *testing.T) {
					srv, st, _ := govEscapeFixture(t, &capStore{})
					if w := tc.send(t, srv, st); w.Code != door.ok {
						t.Fatalf("status = %d, want %d: %s", w.Code, door.ok, w.Body.String())
					}
				})
			}
		})
	}
}

// giveAdminToken puts one wdn_ token for a human admin on st.
func giveAdminToken(st *govEscapeStore, sub string) {
	complete := false
	st.tokenRaw = apiTokenPrefix + "admin-view"
	st.token = &types.APIToken{
		ID: uuid.New(), Principal: sub, Email: "av@corp.example",
		Role: oidc.RoleAdmin, GroupsTruncated: &complete, Name: "cli",
	}
}

// assertAdminViewRefusal pins the 409, its reason and the canon sentence byte
// for byte.
func assertAdminViewRefusal(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	if w.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
	}
	var got errorBody
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", w.Body.String(), err)
	}
	want := errorBody{
		Error:  "Runs start in the user view. Use User view at the top of the console to start one.",
		Reason: "admin_view",
	}
	if got != want {
		t.Errorf("body = %+v, want %+v", got, want)
	}
}
