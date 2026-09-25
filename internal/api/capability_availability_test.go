// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// "Available to" (#612, user-types design 2.6): a restricted value needs an
// allow naming it, at every door a person can reach it through, whatever the
// kind's switch says. Each test here fails with capBatch.decide's step 3
// removed: nothing but the restriction row separates its refused leg from the
// same fixture unrestricted, which every door allows (or, for the widening
// image kind, allows only because the restriction switches it on).

func restrictedOne(kind, value string) map[string]map[string]bool {
	return map[string]map[string]bool{kind: {value: true}}
}

// typeRequest is a launch request from a person of one user type.
func typeRequest(role, userType string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/v1/runs", nil)
	return r.WithContext(utCtx(role, userType, []string{}))
}

// TestAvailability_RestrictedAtEveryLaunchField: the four capability fields
// denyMemberRequest gates (POST /runs and /runs/preflight share it). The
// owner's example: a value listed for Developers only.
func TestAvailability_RestrictedAtEveryLaunchField(t *testing.T) {
	ws := uuid.New()
	const img = "ghcr.io/acme/agent:1.4.2"
	fields := []struct {
		kind, value, reason string
		req                 createRunRequest
	}{
		{capWorkspace, ws.String(), "capability_workspace", createRunRequest{Agent: "claude-code", WorkspaceID: &ws}},
		{capAgent, "codex", "capability_agent", createRunRequest{Agent: "codex"}},
		{capIntegration, "corp-openai", "capability_integration", createRunRequest{Agent: "claude-code", IntegrationID: "corp-openai"}},
		{capImage, img, "byoi_member", createRunRequest{Image: img}},
	}
	for _, f := range fields {
		devAllow := grant(types.CapabilitySubjectUserType, utDev, f.kind, f.value, types.CapabilityAllow)
		legs := []struct {
			name       string
			role, typ  string
			grants     []types.CapabilityGrant
			enf        map[string]bool
			restricted bool
			wantDenied bool
		}{
			{name: "restricted to Developers: a Portfolio manager is refused", role: oidc.RoleUser, typ: utPM,
				grants: []types.CapabilityGrant{devAllow}, restricted: true, wantDenied: true},
			{name: "restricted to Developers: a Developer launches, switch off", role: oidc.RoleUser, typ: utDev,
				grants: []types.CapabilityGrant{devAllow}, restricted: true},
			{name: "restricted, enforced: the listed type launches beside a wildcard allow", role: oidc.RoleUser, typ: utDev,
				grants:     []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", f.kind, capWildcard, types.CapabilityAllow), devAllow},
				enf:        map[string]bool{f.kind: true},
				restricted: true},
			{name: "restricted, enforced: a wildcard allow lists nobody", role: oidc.RoleUser, typ: utPM,
				grants:     []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", f.kind, capWildcard, types.CapabilityAllow), devAllow},
				enf:        map[string]bool{f.kind: true},
				restricted: true, wantDenied: true},
			{name: "restricted and listed, a deny still wins", role: oidc.RoleUser, typ: utDev,
				grants:     []types.CapabilityGrant{devAllow, grant(types.CapabilitySubjectUser, capSub, f.kind, f.value, types.CapabilityDeny)},
				restricted: true, wantDenied: true},
			{name: "a security admin not listed is walled too", role: oidc.RoleSecurityAdmin, typ: utPM,
				grants: []types.CapabilityGrant{devAllow}, restricted: true, wantDenied: true},
			{name: "an admin is exempt", role: oidc.RoleAdmin, typ: utPM,
				grants: []types.CapabilityGrant{devAllow}, restricted: true},
		}
		for _, leg := range legs {
			t.Run(f.kind+"/"+leg.name, func(t *testing.T) {
				h := newHarness(t)
				cs := &capStore{grants: leg.grants, enf: leg.enf, userTypes: utKnown}
				if leg.restricted {
					cs.restricted = restrictedOne(f.kind, f.value)
				}
				h.srv.cfg.Store = cs
				w := httptest.NewRecorder()
				_, denied := h.srv.denyMemberRequest(w, typeRequest(leg.role, leg.typ), f.req)
				if denied != leg.wantDenied {
					t.Fatalf("denied = %v, want %v (status %d: %s)", denied, leg.wantDenied, w.Code, w.Body.String())
				}
				if !leg.wantDenied {
					return
				}
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403", w.Code)
				}
				if reasons := auditReasons(t, h.srv, "authz.denied"); !slices.Equal(reasons, []string{f.reason}) {
					t.Fatalf("authz.denied reasons = %v, want [%s]", reasons, f.reason)
				}
			})
		}
	}
}

// TestAvailability_RestrictedWorkspaceRepoIsDropped: the inline-policy seam
// drops and proceeds, as it does for any ungranted workspace repo — the run
// launches without the item, Review names it, and the drop is audited.
func TestAvailability_RestrictedWorkspaceRepoIsDropped(t *testing.T) {
	const repo = "octocat/Hello-World"
	ws := types.Workspace{ID: uuid.New(), Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeRepo, Source: repo}}}
	authored := types.RunPolicySpec{
		MinConfinementClass: types.CC2,
		WorkspaceRepos:      []types.WorkspaceRepo{{Repo: repo, Target: "/work/repo"}},
	}
	for _, tc := range []struct {
		name     string
		listType string
		wantKept bool
	}{
		{"restricted to another type: dropped", utDev, false},
		{"restricted to this caller's type: kept", types.UserTypeStandard, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newSecretsHarness(t)
			h.srv.cfg.Store = &capStore{
				Store:      wsRefStore{ws: []types.Workspace{ws}},
				grants:     []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, tc.listType, capWorkspace, ws.ID.String(), types.CapabilityAllow)},
				restricted: restrictedOne(capWorkspace, ws.ID.String()),
				userTypes:  utKnown,
			}
			h.srv.cfg.DefaultPolicy = types.RunPolicySpec{MinConfinementClass: types.CC2}
			got, warns := resolveInline(t, h, authored)
			if kept := len(got.WorkspaceRepos) == 1; kept != tc.wantKept {
				t.Fatalf("repos = %v, want kept=%v", got.WorkspaceRepos, tc.wantKept)
			}
			if tc.wantKept {
				return
			}
			if d := dropWarns(warns); len(d) != 1 || !strings.Contains(d[0], repo) {
				t.Fatalf("drop warnings = %v, want one naming the dropped repo", d)
			}
			if reasons := auditReasons(t, h.srv, "authz.denied"); !slices.Equal(reasons, []string{"capability_workspace"}) {
				t.Fatalf("authz.denied reasons = %v, want [capability_workspace]", reasons)
			}
		})
	}
}

// TestAvailability_RestrictedProviderAtEveryDoor: the six doors a repository
// reaches a clone through, with the git provider row restricted and the kind
// NOT enforced.
func TestAvailability_RestrictedProviderAtEveryDoor(t *testing.T) {
	restricted := restrictedOne(capWorkspaceProvider, capProviderRowID)
	for _, door := range providerDoors() {
		t.Run(door.name, func(t *testing.T) {
			t.Run("restricted, not listed: 403", func(t *testing.T) {
				srv, w := door.fire(t, &capStore{restricted: restricted, grants: capProviderAllow("someone-else")},
					capProviderSite(), door.member(t))
				assertProviderDenied(t, srv, w)
			})
			t.Run("restricted, listed: not refused", func(t *testing.T) {
				_, w := door.fire(t, &capStore{restricted: restricted, grants: capProviderAllow(door.sub)},
					capProviderSite(), door.member(t))
				assertNotRefused(t, w, "a person listed for the row may bring work from it")
			})
		})
	}
}

// TestAvailability_RestrictedAgentRefusesHarnessLogin: the per-person model
// sign-in is gated on capAgent for the roster row's agent.
func TestAvailability_RestrictedAgentRefusesHarnessLogin(t *testing.T) {
	row := perUserAWSRow()
	for _, tc := range []struct {
		name     string
		listType string
		wantOK   bool
	}{
		{"restricted to another type: refused", utDev, false},
		{"restricted to this caller's type: allowed", utPM, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := &capStore{
				grants:     []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, tc.listType, capAgent, row.ID, types.CapabilityAllow)},
				restricted: restrictedOne(capAgent, row.ID),
				userTypes:  utKnown,
			}
			srv := New(baseTestConfig(newHarness(t), &integStore{govEscapeStore: newGovEscapeStore(cs), site: agentRoster(row)}))
			w := httptest.NewRecorder()
			_, _, ok := srv.authorizeHarnessLogin(w, typeRequest(oidc.RoleUser, utPM), awsSSOProvider)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (status %d: %s)", ok, tc.wantOK, w.Code, w.Body.String())
			}
			if !tc.wantOK && w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", w.Code)
			}
		})
	}
}

// TestAvailability_RestrictedReadFailureNeverAllows: capRead.enforced folds a
// failed capability_restrictions read into r.err, and decide discards
// whatever its steps computed once r.err is set — so a store outage on a
// NARROWING, unenforced kind (the leg that would otherwise fall through to
// "not restricted, so allowed" at step 6) can never resolve to an allow.
// Pinned at both the resolver (decide's own error) and the door
// (denyMemberRequest, which must answer 403 or 500, never launch).
func TestAvailability_RestrictedReadFailureNeverAllows(t *testing.T) {
	ws := uuid.New()
	cs := &capStore{
		restricted:  restrictedOne(capWorkspace, ws.String()),
		restrictErr: errors.New("boom"),
		userTypes:   utKnown,
	}
	h := newHarness(t)
	h.srv.cfg.Store = cs

	ctx := withCapBatch(typeRequest(oidc.RoleUser, utPM).Context())
	if _, err := h.srv.capSeamAllowed(ctx, capWorkspace, ws.String()); err == nil {
		t.Fatal("capSeamAllowed = nil error, want the restriction read's failure to propagate")
	}

	w := httptest.NewRecorder()
	req := typeRequest(oidc.RoleUser, utPM)
	_, denied := h.srv.denyMemberRequest(w, req, createRunRequest{Agent: "claude-code", WorkspaceID: &ws})
	if !denied {
		t.Fatalf("denyMemberRequest allowed the launch on a failed restriction read (status %d: %s)", w.Code, w.Body.String())
	}
	if w.Code != http.StatusForbidden && w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 403 or 500", w.Code)
	}
}

// TestAvailabilityRoutes: GET/PUT /permissions/availability/{kind}/{value}.
func TestAvailabilityRoutes(t *testing.T) {
	const img = "ghcr.io/acme/agent:1.4.2"
	path := "/api/v1/permissions/availability/image/" + img
	decode := func(t *testing.T, w *httptest.ResponseRecorder) availabilityView {
		t.Helper()
		var v availabilityView
		if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
			t.Fatalf("decode %s: %v", w.Body.String(), err)
		}
		return v
	}

	t.Run("Only with nobody listed is refused, and nothing is written", func(t *testing.T) {
		srv, st := permServer(t)
		w := doSSO(t, srv, http.MethodPut, path, permAdmin(t), `{"restricted":true}`)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), availabilityOnlyEmptyMsg) {
			t.Fatalf("status = %d %s, want 400 with the empty-list sentence", w.Code, w.Body.String())
		}
		if len(st.restricted[capImage]) != 0 {
			t.Fatalf("restricted = %v, want nothing written", st.restricted)
		}
	})

	t.Run("a wildcard allow does not count as a listing", func(t *testing.T) {
		srv, st := permServer(t)
		st.grants = []types.CapabilityGrant{grant(types.CapabilitySubjectAll, "", capImage, capWildcard, types.CapabilityAllow)}
		if w := doSSO(t, srv, http.MethodPut, path, permAdmin(t), `{"restricted":true}`); w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d %s, want 400", w.Code, w.Body.String())
		}
	})

	t.Run("listed: turned on, read back, audited, turned off", func(t *testing.T) {
		srv, st := permServer(t)
		audit := &recRecorder{}
		srv.cfg.Audit = audit
		allow := grant(types.CapabilitySubjectUserType, utDev, capImage, img, types.CapabilityAllow)
		st.grants = []types.CapabilityGrant{allow, grant(types.CapabilitySubjectUser, "x", capImage, "other:1", types.CapabilityAllow)}
		w := doSSO(t, srv, http.MethodPut, path, permAdmin(t), `{"restricted":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("PUT status = %d %s", w.Code, w.Body.String())
		}
		if !st.restricted[capImage][img] {
			t.Fatalf("restricted = %v, want %s restricted", st.restricted, img)
		}
		v := decode(t, doSSO(t, srv, http.MethodGet, path, permAdmin(t), ""))
		if !v.Restricted || v.Kind != capImage || v.Value != img || len(v.AllowedBy) != 1 || v.AllowedBy[0].ID != allow.ID {
			t.Fatalf("GET = %+v, want restricted with the one allow naming it", v)
		}
		var rows []map[string]any
		for _, ev := range audit.snapshot() {
			if ev.Action == "capability.availability.write" {
				var d map[string]any
				_ = json.Unmarshal(ev.Data, &d)
				rows = append(rows, d)
			}
		}
		if len(rows) != 1 || rows[0]["kind"] != capImage || rows[0]["value"] != img || rows[0]["restricted"] != true {
			t.Fatalf("capability.availability.write rows = %v, want one {kind image, value, restricted true}", rows)
		}
		if w := doSSO(t, srv, http.MethodPut, path, permAdmin(t), `{"restricted":false}`); w.Code != http.StatusOK || st.restricted[capImage][img] {
			t.Fatalf("turn off = %d %s, restricted = %v", w.Code, w.Body.String(), st.restricted)
		}
	})

	for _, bad := range []struct{ name, path, body string }{
		{"a kind that can't be restricted", "/api/v1/permissions/availability/egress_host/pypi.org", `{"restricted":false}`},
		{"an unknown kind", "/api/v1/permissions/availability/devcontainer_repo/x", `{"restricted":false}`},
		{"the wildcard", "/api/v1/permissions/availability/agent/*", `{"restricted":false}`},
		{"a workspace that is not a uuid", "/api/v1/permissions/availability/workspace/nope", `{"restricted":false}`},
		{"no restricted field", path, `{}`},
	} {
		t.Run("400: "+bad.name, func(t *testing.T) {
			srv, _ := permServer(t)
			if w := doSSO(t, srv, http.MethodPut, bad.path, permAdmin(t), bad.body); w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d %s, want 400", w.Code, w.Body.String())
			}
		})
	}

	t.Run("a user is refused", func(t *testing.T) {
		srv, _ := permServer(t)
		if w := doSSO(t, srv, http.MethodGet, path, permMember(t), ""); w.Code != http.StatusForbidden {
			t.Fatalf("status = %d, want 403", w.Code)
		}
	})
}
