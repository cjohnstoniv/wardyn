// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// TestMeServiceReach: a service identity's GET /me carries a `service` block whose routes are the allow-list
// and whose presets and workspaces are exactly what GET /presets and GET /workspaces return to that token.
// A person's /me never carries the key.
func TestMeServiceReach(t *testing.T) {
	e := newSvcEnv(t)
	ctx := context.Background()
	for _, ut := range []string{svcType, svcType2} {
		if _, err := e.st.CreateUserType(ctx, types.UserType{ID: ut, Name: strings.ToUpper(ut)}); err != nil {
			t.Fatalf("seed user type %s: %v", ut, err)
		}
	}
	e.mustCreateTyped(t, svcName, svcSponsor, svcType)
	e.mustCreateTyped(t, svcName2, svcSponsor2, svcType2)
	tok := e.mustMint(t, e.sponsor, svcName).Token
	get := func(path string) *httptest.ResponseRecorder { return e.as(t, tok, http.MethodGet, path, "") }

	for name, uts := range map[string][]string{
		"open-all": nil, "open-ci": {svcType}, "open-ci2": {svcType2}, "open-sponsor": {"standard"},
	} {
		body, _ := json.Marshal(client.PresetRequest{UserTypes: uts, Request: client.CreateRunRequest{Agent: "claude-code"}})
		if w := do(t, e.h.srv, http.MethodPut, "/api/v1/presets/"+name, adminToken, string(body)); w.Code != http.StatusCreated {
			t.Fatalf("put preset %s = %d %s", name, w.Code, w.Body.String())
		}
	}
	ephemeral := []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}}
	created := time.Now().UTC().Add(-time.Hour)
	mkWS := func(name, owner string) uuid.UUID {
		t.Helper()
		created = created.Add(time.Minute)
		ws, err := e.st.CreateWorkspace(ctx, types.Workspace{ID: uuid.New(), Name: name, OwnedBy: owner,
			Status: types.WorkspaceScanned, Sources: ephemeral, CreatedAt: created, UpdatedAt: created})
		if err != nil {
			t.Fatalf("create workspace %s: %v", name, err)
		}
		return ws.ID
	}
	wsOpen := mkWS("operator-open", "")
	wsOwn := mkWS("service-owned", svcPrin)
	mkWS("second-identity", svcPrin2)
	mkWS("sponsor-owned", svcSponsor)
	wsRestricted := mkWS("operator-restricted", "")
	if err := e.st.SetCapabilityRestriction(ctx, capWorkspace, wsRestricted.String(), true, "test"); err != nil {
		t.Fatal(err)
	}

	reachOf := func(t *testing.T) meServiceReach {
		t.Helper()
		w := get("/api/v1/me")
		var body struct {
			Principal string          `json:"principal"`
			Service   *meServiceReach `json:"service"`
		}
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil || body.Service == nil {
			t.Fatalf("service GET /me = %d %s, want 200 with a service block", w.Code, w.Body.String())
		}
		if body.Principal != svcPrin {
			t.Errorf("principal = %q, want %q", body.Principal, svcPrin)
		}
		return *body.Service
	}

	t.Run("identity", func(t *testing.T) {
		if r := reachOf(t); r.Name != svcName || r.Sponsor != svcSponsor {
			t.Errorf("name/sponsor = %q/%q, want %q/%q", r.Name, r.Sponsor, svcName, svcSponsor)
		}
	})
	t.Run("routes_are_the_allow_list", func(t *testing.T) {
		// TestServiceIdentity_AllowListRouteWalk pins serviceAllowed to its wantAllowed list.
		if got, want := reachOf(t).Routes, slices.Sorted(maps.Keys(serviceAllowed)); !slices.Equal(got, want) || len(got) != 16 {
			t.Errorf("routes = %v, want the 16 allow-list keys %v", got, want)
		}
	})
	t.Run("presets_equal_the_route", func(t *testing.T) {
		w := get("/api/v1/presets")
		var doc client.PresetsDocument
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &doc) != nil {
			t.Fatalf("GET /presets = %d %s", w.Code, w.Body.String())
		}
		want := []meReachPreset{}
		for _, p := range doc.Presets {
			want = append(want, meReachPreset{Name: p.Name, Version: p.Version})
		}
		if got := reachOf(t).Presets; !slices.Equal(got, want) {
			t.Errorf("reach presets = %v, GET /presets = %v", got, want)
		}
		if got := listedPresets(t, w); !slices.Equal(got, []string{"open-all", "open-ci"}) {
			t.Errorf("GET /presets = %v, want open-all and open-ci (the parity must not be trivial)", got)
		}
	})
	t.Run("workspaces_equal_the_route", func(t *testing.T) {
		w := get("/api/v1/workspaces")
		var rows []types.Workspace
		if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &rows) != nil {
			t.Fatalf("GET /workspaces = %d %s", w.Code, w.Body.String())
		}
		want := []meReachWorkspace{}
		for _, ws := range rows {
			want = append(want, meReachWorkspace{ID: ws.ID.String(), Name: ws.Name})
		}
		if got := reachOf(t).Workspaces; !slices.Equal(got, want) {
			t.Errorf("reach workspaces = %v, GET /workspaces = %v", got, want)
		}
		if got := listedWorkspaces(t, w); !slices.Equal(got, []uuid.UUID{wsOwn, wsOpen}) {
			t.Errorf("GET /workspaces = %v, want %s and %s (the parity must not be trivial)", got, wsOwn, wsOpen)
		}
	})
	t.Run("person_has_no_service_key", func(t *testing.T) {
		for who, w := range map[string]*httptest.ResponseRecorder{
			"sponsor":     doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me", e.sponsor, ""),
			"admin":       doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me", e.admin, ""),
			"admin token": do(t, e.h.srv, http.MethodGet, "/api/v1/me", adminToken, ""),
		} {
			var body map[string]json.RawMessage
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &body) != nil {
				t.Fatalf("%s GET /me = %d %s", who, w.Code, w.Body.String())
			}
			if _, ok := body["service"]; ok {
				t.Errorf("%s GET /me carries a service key: %s", who, w.Body.String())
			}
		}
	})
	// Last: it swaps the store.
	t.Run("site_config_unreadable", func(t *testing.T) {
		prev := e.h.srv.cfg.Store
		e.h.srv.cfg.Store = siteConfigUnreadable{e.st}
		t.Cleanup(func() { e.h.srv.cfg.Store = prev })
		me, list := get("/api/v1/me"), get("/api/v1/workspaces")
		if me.Code != http.StatusServiceUnavailable || respReason(me) != reasonRosterUnreadable || me.Body.String() != list.Body.String() {
			t.Errorf("service GET /me with an unreadable site config = %d %s, want GET /workspaces' 503 %s", me.Code, me.Body.String(), list.Body.String())
		}
		if w := doSSO(t, e.h.srv, http.MethodGet, "/api/v1/me", e.sponsor, ""); w.Code != http.StatusOK {
			t.Errorf("a person's GET /me with an unreadable site config = %d %s, want 200", w.Code, w.Body.String())
		}
	})
}

// TestMeServiceReach_PersonBodyGolden: a person's GET /me body is byte-identical to the one before the
// service block existed (the goldens were taken from that handler).
func TestMeServiceReach_PersonBodyGolden(t *testing.T) {
	member := withOIDCRole(withOIDCName(withOIDCEmail(withOIDCHuman(context.Background(), "gsv-member-0001"),
		"alice.smith@corp.example"), "Alice Smith"), oidc.RoleUser)
	for name, tc := range map[string]struct {
		ctx  context.Context
		want string
	}{
		"sso member":  {member, `{"email":"alice.smith@corp.example","governance_contact":{"source":"deployment"},"member_local_dir_root":null,"method":"token","name":"Alice Smith","operator":false,"principal":"gsv-member-0001","role":"user","security_operator":false,"user_drive":null,"user_drive_denied_by_profile":"","user_drive_unavailable":"","user_preview_available":false,"user_type":null,"user_view":false,"user_view_dropped":null,"user_view_no_credential":false,"user_view_preselect_type":"","user_view_types":null}` + "\n"},
		"admin token": {context.Background(), `{"email":"","governance_contact":null,"member_local_dir_root":null,"method":"token","name":"","operator":true,"principal":"admin-token","role":"admin","security_operator":true,"user_drive":null,"user_drive_denied_by_profile":"","user_drive_unavailable":"","user_preview_available":false,"user_type":null,"user_view":false,"user_view_dropped":null,"user_view_no_credential":false,"user_view_preselect_type":"","user_view_types":null}` + "\n"},
	} {
		t.Run(name, func(t *testing.T) {
			w := httptest.NewRecorder()
			(&Server{}).handleMe(w, httptest.NewRequest(http.MethodGet, "/api/v1/me", nil).WithContext(tc.ctx))
			if got := w.Body.String(); got != tc.want {
				t.Errorf("GET /me body =\n%s\nwant\n%s", got, tc.want)
			}
		})
	}
}
