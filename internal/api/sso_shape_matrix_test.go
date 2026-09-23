// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSSOShapeRoleMatrix is the role × deployment-shape matrix at the API
// level: every role, against every SSO deployment shape Wardyn ships, over the
// SAME chi.Walk-proven routeMatrix TestAuthzMatrix executes once.
//
// The shapes differ only in the three knobs the daemon reads at this layer —
// the admin token, WARDYN_SSO_ONLY and WARDYN_USER_DESKTOP — so each is that
// config over the maximally-mounted matrix server. Which role a sign-in
// DERIVES on each shape is the other half, pinned against the shipped config
// files by internal/auth/oidc's TestShippedShapeRoleDerivation; the live role
// walk (ui/e2e/live/sso-roles.spec.ts, via scripts/kind-sso-walk.sh and
// scripts/compose-sso-roles.sh) proves sign-in → role → console on each shape.
//
// member2 exists for cross-member isolation: it owns nothing, so every
// owner-scoped route must answer it the byte-identical 404 a missing entity
// gets, on every shape.
func TestSSOShapeRoleMatrix(t *testing.T) {
	shapes := []ssoShapeCase{
		{name: "chart SSO + admin token (and compose --profile sso)", shape: func(*Config) {}, tokenLogin: true, tokenWorks: true},
		{name: "chart auth.ssoOnly", shape: func(c *Config) { c.AdminToken = ""; c.SSOOnly = true }, ssoOnly: true},
		// m′: the token is the MDM-held process credential — it still works as a
		// bearer, but the console never offers it as a human sign-in.
		{name: "desktop member mode (m′)", shape: func(c *Config) { c.MemberMode = true }, tokenWorks: true},
	}
	cast := ssoShapeCast{
		admin:   ssoPersona{name: "admin", role: oidc.RoleAdmin, operator: true, security: true, cookie: ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)},
		sec:     ssoPersona{name: "security_admin", role: oidc.RoleSecurityAdmin, security: true, cookie: ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)},
		member:  ssoPersona{name: "member", role: oidc.RoleMember, cookie: ssoSession(t, ssoShapeMemberSub, "member@corp.example", oidc.RoleMember)},
		member2: ssoPersona{name: "member2", role: oidc.RoleMember, cookie: ssoSession(t, "sub-member2", "member2@corp.example", oidc.RoleMember)},
	}
	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			srv, ast, aap, rs := newAuthzMatrixServer(t, sh.shape)
			checkShapePosture(t, srv, sh, cast)
			seed := func(e routeEntity) uuid.UUID { return seedOwnedBy(ast, aap, rs, e, ssoShapeMemberSub) }
			for key, rc := range routeMatrix {
				method, pattern, _ := strings.Cut(key, " ")
				switch rc.class {
				case classAdmin, classSecurity:
					t.Run(key, func(t *testing.T) { probeGatedRoute(t, srv, method, pattern, rc, cast) })
				case classMember:
					t.Run(key, func(t *testing.T) { probeMemberRoute(t, srv, method, pattern, rc, cast) })
				case classOwner:
					t.Run(key, func(t *testing.T) { probeOwnerRoute(t, srv, method, pattern, rc, cast, seed) })
				}
			}
		})
	}
}

const ssoShapeMemberSub = "sub-member"

type ssoShapeCase struct {
	name  string
	shape func(*Config)
	// what /healthz tells the sign-in screen
	tokenLogin, ssoOnly bool
	// whether the admin-token principal exists at all
	tokenWorks bool
}

type ssoPersona struct {
	name, role         string
	operator, security bool
	cookie             *http.Cookie
}

type ssoShapeCast struct{ admin, sec, member, member2 ssoPersona }

func (c ssoShapeCast) all() []ssoPersona { return []ssoPersona{c.admin, c.sec, c.member, c.member2} }

func decodeJSONBody(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return m
}

// checkShapePosture: what /healthz tells the sign-in screen, whether the
// admin-token principal exists, and the role each session reports.
func checkShapePosture(t *testing.T, srv *Server, sh ssoShapeCase, cast ssoShapeCast) {
	t.Helper()
	hz := decodeJSONBody(t, doSSO(t, srv, http.MethodGet, "/healthz", nil, "").Body.Bytes())
	if hz["token_login"] != sh.tokenLogin || hz["sso_only"] != sh.ssoOnly {
		t.Errorf("/healthz token_login=%v sso_only=%v, want %v/%v", hz["token_login"], hz["sso_only"], sh.tokenLogin, sh.ssoOnly)
	}

	me := do(t, srv, http.MethodGet, "/api/v1/me", adminToken, "")
	site := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
	switch {
	case !sh.tokenWorks && (me.Code != http.StatusUnauthorized || site.Code != http.StatusUnauthorized):
		t.Errorf("no token configured: /me=%d /site-config=%d, want 401/401", me.Code, site.Code)
	case sh.tokenWorks:
		body := decodeJSONBody(t, me.Body.Bytes())
		if me.Code != http.StatusOK || body["role"] != oidc.RoleAdmin || body["method"] != "token" || body["operator"] != true {
			t.Errorf("token /me = %d %v, want 200 role=admin method=token operator=true", me.Code, body)
		}
		assertNotBlocked(t, "admin token on an admin route", site)
	}

	for _, p := range cast.all() {
		w := doSSO(t, srv, http.MethodGet, "/api/v1/me", p.cookie, "")
		body := decodeJSONBody(t, w.Body.Bytes())
		if w.Code != http.StatusOK || body["role"] != p.role || body["method"] != "sso" ||
			body["operator"] != p.operator || body["security_operator"] != p.security {
			t.Errorf("%s /me = %d %v, want role=%s operator=%v security_operator=%v",
				p.name, w.Code, body, p.role, p.operator, p.security)
		}
	}
}

func probeGatedRoute(t *testing.T, srv *Server, method, pattern string, rc classifiedRoute, cast ssoShapeCast) {
	t.Helper()
	body, p := bodyFor(method, rc), buildPath(pattern, "x1")
	assertNotBlocked(t, "admin", doSSO(t, srv, method, p, cast.admin.cookie, body))
	w := doSSO(t, srv, method, p, cast.sec.cookie, body)
	if rc.class == classSecurity {
		assertNotBlocked(t, "security_admin", w)
	} else if w.Code != http.StatusForbidden {
		t.Errorf("security_admin on a super route: %d, want 403", w.Code)
	}
	for _, m := range []ssoPersona{cast.member, cast.member2} {
		if w := doSSO(t, srv, method, p, m.cookie, body); w.Code != http.StatusForbidden {
			t.Errorf("%s: %d, want 403; body=%s", m.name, w.Code, w.Body.String())
		}
	}
}

func probeMemberRoute(t *testing.T, srv *Server, method, pattern string, rc classifiedRoute, cast ssoShapeCast) {
	t.Helper()
	body, p := bodyFor(method, rc), buildPath(pattern, uuid.New().String())
	for _, pr := range cast.all() {
		assertNotBlocked(t, pr.name, doSSO(t, srv, method, p, pr.cookie, body))
	}
}

// probeOwnerRoute seeds a FRESH member-owned entity per probe: decide, kill and
// delete mutate what they touch, so no two probes share one.
func probeOwnerRoute(t *testing.T, srv *Server, method, pattern string, rc classifiedRoute, cast ssoShapeCast, seed func(routeEntity) uuid.UUID) {
	t.Helper()
	body := bodyFor(method, rc)
	on := func(p ssoPersona) int {
		return doSSO(t, srv, method, buildPath(pattern, seed(rc.entity).String()), p.cookie, body).Code
	}
	reached := func(code int) bool {
		return code != http.StatusUnauthorized && code != http.StatusForbidden && code != http.StatusNotFound
	}
	if code := on(cast.member); !reached(code) {
		t.Errorf("owning member: %d, want the handler's own answer", code)
	}
	// Cross-member isolation: the existence-oracle 404, never 403.
	if code := on(cast.member2); code != http.StatusNotFound {
		t.Errorf("member2 on member's entity: %d, want 404", code)
	}
	if code := on(cast.admin); !reached(code) {
		t.Errorf("admin on a foreign entity: %d, want the handler's own answer", code)
	}
	code := on(cast.sec)
	if rc.ownerTier == tierSuper && code != http.StatusNotFound {
		t.Errorf("security_admin on a foreign tierSuper entity: %d, want 404", code)
	} else if rc.ownerTier == tierSecurity && !reached(code) {
		t.Errorf("security_admin on a foreign tierSecurity entity: %d, want the handler's own answer", code)
	}
}

func seedOwnedBy(ast *authzStore, aap *authzApprovals, rs *recording.FSStore, e routeEntity, owner string) uuid.UUID {
	id := uuid.New()
	ast.mu.Lock()
	if e == entityWorkspace {
		ast.workspaces[id] = types.Workspace{
			ID: id, Name: "ws-shape", OwnedBy: owner, Status: types.WorkspaceScanned,
			Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
		}
		ast.mu.Unlock()
		return id
	}
	ast.runs[id] = types.AgentRun{ID: id, CreatedBy: owner, State: types.RunRunning, Agent: "claude-code"}
	ast.mu.Unlock()
	_ = rs.SaveCast(context.Background(), id.String(), strings.NewReader(`{"version":2}`+"\n"))
	if e == entityApproval {
		return aap.seed(id)
	}
	return id
}
