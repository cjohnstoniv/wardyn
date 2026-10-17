// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const previewSub = "sub-uv-preview"

// uvPreviewServer is govEscapeFixture with WARDYN_GOVERN_ADMIN_RUNS set to
// govern and the user types the preview looks through.
func uvPreviewServer(t *testing.T, govern bool) (*Server, *govEscapeStore, *recRecorder) {
	t.Helper()
	h := newHarness(t)
	st := newGovEscapeStore(&capStore{userTypes: utKnown})
	audit := &recRecorder{}
	cfg := baseTestConfig(h, st)
	cfg.Audit = audit
	cfg.Broker = h.broker
	cfg.Runner = &fakeRunner{}
	cfg.Secrets = &memSecrets{m: map[string][]byte{govCorpSecret: []byte("v")}}
	cfg.OIDC = &oidc.Authenticator{}
	cfg.DefaultPolicy = govDeployment()
	cfg.GovernAdminRuns = govern
	return New(cfg), st, audit
}

func TestUserViewPreview_RefusesWrites(t *testing.T) {
	const runBody = `{"agent":"claude-code","task":"t"}`
	for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin} {
		for _, stamped := range []string{types.UserTypeStandard, utPM} {
			t.Run(role+" stamped "+stamped, func(t *testing.T) {
				srv, st, audit := uvPreviewServer(t, true)
				view := uvSession(t, previewSub, role, stamped, utDev)
				for _, door := range []struct{ method, path, body string }{
					{http.MethodPost, "/api/v1/runs", runBody},
					{http.MethodPost, "/api/v1/runs/preflight", runBody},
					{http.MethodPost, "/api/v1/workspaces", `{"name":"w"}`},
					{http.MethodPut, "/api/v1/me/run-layout", `{}`},
				} {
					w := doSSO(t, srv, door.method, door.path, view, door.body)
					var body errorBody
					if w.Code != http.StatusConflict || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Reason != "user_view_preview" {
						t.Fatalf("%s %s = %d %s, want 409 user_view_preview", door.method, door.path, w.Code, w.Body.String())
					}
					want := "The User view is looking through the developer user type, a read-only preview. " +
						"Switch the view to your own type (" + stamped + ") to make changes or launch."
					if body.Error != want {
						t.Errorf("sentence = %q, want %q", body.Error, want)
					}
					var data map[string]any
					ev := lastAuditEvent(t, audit.snapshot(), "authz.denied")
					if err := json.Unmarshal(ev.Data, &data); err != nil || data["reason"] != "user_view_preview" ||
						data["viewed_user_type"] != utDev || data["stamped_user_type"] != stamped ||
						data["user_type"] != utDev || data["user_view"] != true || data["method"] != door.method {
						t.Fatalf("authz.denied = %s, want user_view_preview with the viewed and stamped types", ev.Data)
					}
				}
				if len(st.runs) != 0 {
					t.Errorf("runs created = %d, want 0", len(st.runs))
				}
			})
		}
	}
}

func TestUserViewPreview_ReadsAndExitsStillWork(t *testing.T) {
	for _, role := range []string{oidc.RoleAdmin, oidc.RoleSecurityAdmin} {
		t.Run(role, func(t *testing.T) {
			srv, _, _ := uvPreviewServer(t, true)
			view := uvSession(t, previewSub, role, types.UserTypeStandard, utDev)
			for _, door := range []struct{ method, path, body string }{
				{http.MethodGet, "/api/v1/me", ""},
				{http.MethodGet, "/api/v1/me/capabilities", ""},
				{http.MethodPost, "/api/v1/me/view", `{"view":"admin"}`},
				{http.MethodPost, "/api/v1/auth/logout", ""},
				{http.MethodPost, "/api/v1/policies/grade", `{"spec":{"min_confinement_class":"CC1"}}`},
			} {
				w := doSSO(t, srv, door.method, door.path, view, door.body)
				if w.Code >= 400 {
					t.Errorf("%s %s = %d %s, want no refusal", door.method, door.path, w.Code, w.Body.String())
				}
			}
		})
	}
}

func TestUserViewPreview_OwnTypeLaunches(t *testing.T) {
	for _, stamped := range []string{types.UserTypeStandard, utDev} {
		t.Run("stamped "+stamped, func(t *testing.T) {
			srv, st, _ := uvPreviewServer(t, true)
			view := uvSession(t, previewSub, oidc.RoleAdmin, stamped, stamped)
			if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", view, `{"agent":"claude-code","task":"t"}`); w.Code != http.StatusCreated {
				t.Fatalf("POST /runs viewing the own type = %d %s, want 201", w.Code, w.Body.String())
			}
			if len(st.runs) != 1 {
				t.Errorf("runs created = %d, want 1", len(st.runs))
			}
		})
	}
}

// With the switch unset the preview gate is inert: another type's view
// launches as it did before.
func TestUserViewPreview_SwitchOffIsUnchanged(t *testing.T) {
	srv, _, _ := uvPreviewServer(t, false)
	view := uvSession(t, previewSub, oidc.RoleAdmin, types.UserTypeStandard, utDev)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/runs", view, `{"agent":"claude-code","task":"t"}`); w.Code != http.StatusCreated {
		t.Fatalf("POST /runs in another type's view, switch off = %d %s, want 201", w.Code, w.Body.String())
	}
}
