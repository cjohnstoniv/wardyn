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
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestSSOShapeRoleMatrix is the role × deployment-shape matrix at the API
// level: every role, against every SSO deployment shape Wardyn ships, over the
// SAME chi.Walk-proven routeMatrix TestAuthzMatrix executes once.
//
// The shapes differ only in the three knobs the daemon reads at this layer —
// the admin token, WARDYN_SSO_ONLY and WARDYN_MEMBER_MODE — so each is that
// config over the maximally-mounted matrix server. Which role a sign-in
// DERIVES on each shape is the other half, pinned against the shipped config
// files by internal/auth/oidc's TestShippedShapeRoleDerivation; the live kind
// walk (ui/e2e/live/sso-roles.spec.ts) proves sign-in → role → console on the
// two chart renders.
//
// member2 exists for cross-member isolation: it owns nothing, so every
// owner-scoped route must answer it the byte-identical 404 a missing entity
// gets, on every shape.
func TestSSOShapeRoleMatrix(t *testing.T) {
	shapes := []struct {
		name  string
		shape func(*Config)
		// what /healthz tells the sign-in screen
		tokenLogin, ssoOnly bool
		// whether the admin-token principal exists at all
		tokenWorks bool
	}{
		{name: "chart SSO + admin token", shape: func(*Config) {}, tokenLogin: true, tokenWorks: true},
		{name: "chart auth.ssoOnly", shape: func(c *Config) { c.AdminToken = ""; c.SSOOnly = true }, ssoOnly: true},
		// m′: the token is the MDM-held process credential — it still works as a
		// bearer, but the console never offers it as a human sign-in.
		{name: "desktop member mode (m′)", shape: func(c *Config) { c.MemberMode = true }, tokenWorks: true},
	}

	const memberSub, member2Sub = "sub-member", "sub-member2"
	type persona struct {
		name, role             string
		operator, securityOper bool
		cookie                 *http.Cookie
	}
	personas := []persona{
		{name: "admin", role: oidc.RoleAdmin, operator: true, securityOper: true,
			cookie: ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)},
		{name: "security_admin", role: oidc.RoleSecurityAdmin, securityOper: true,
			cookie: ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)},
		{name: "member", role: oidc.RoleMember,
			cookie: ssoSession(t, memberSub, "member@corp.example", oidc.RoleMember)},
		{name: "member2", role: oidc.RoleMember,
			cookie: ssoSession(t, member2Sub, "member2@corp.example", oidc.RoleMember)},
	}
	admin, sec, member, member2 := personas[0].cookie, personas[1].cookie, personas[2].cookie, personas[3].cookie

	for _, sh := range shapes {
		t.Run(sh.name, func(t *testing.T) {
			srv, ast, aap, rs := newAuthzMatrixServer(t, sh.shape)
			decode := func(t *testing.T, body []byte) map[string]any {
				t.Helper()
				var m map[string]any
				if err := json.Unmarshal(body, &m); err != nil {
					t.Fatalf("decode %s: %v", body, err)
				}
				return m
			}

			t.Run("sign-in screen posture", func(t *testing.T) {
				hz := decode(t, doSSO(t, srv, http.MethodGet, "/healthz", nil, "").Body.Bytes())
				if hz["token_login"] != sh.tokenLogin || hz["sso_only"] != sh.ssoOnly {
					t.Errorf("/healthz token_login=%v sso_only=%v, want %v/%v", hz["token_login"], hz["sso_only"], sh.tokenLogin, sh.ssoOnly)
				}
			})

			t.Run("admin-token principal", func(t *testing.T) {
				me := do(t, srv, http.MethodGet, "/api/v1/me", adminToken, "")
				site := do(t, srv, http.MethodGet, "/api/v1/site-config", adminToken, "")
				if !sh.tokenWorks {
					if me.Code != http.StatusUnauthorized || site.Code != http.StatusUnauthorized {
						t.Errorf("no token configured: /me=%d /site-config=%d, want 401/401", me.Code, site.Code)
					}
					return
				}
				body := decode(t, me.Body.Bytes())
				if me.Code != http.StatusOK || body["role"] != oidc.RoleAdmin || body["method"] != "token" || body["operator"] != true {
					t.Errorf("token /me = %d %v, want 200 role=admin method=token operator=true", me.Code, body)
				}
				assertNotBlocked(t, "admin token on an admin route", site)
			})

			t.Run("session role", func(t *testing.T) {
				for _, p := range personas {
					w := doSSO(t, srv, http.MethodGet, "/api/v1/me", p.cookie, "")
					body := decode(t, w.Body.Bytes())
					if w.Code != http.StatusOK || body["role"] != p.role || body["method"] != "sso" ||
						body["operator"] != p.operator || body["security_operator"] != p.securityOper {
						t.Errorf("%s /me = %d %v, want role=%s operator=%v security_operator=%v",
							p.name, w.Code, body, p.role, p.operator, p.securityOper)
					}
				}
			})

			// owned seeds a FRESH entity owned by member per probe: decide, kill
			// and delete mutate what they touch, so no two probes share one.
			owned := func(e routeEntity) uuid.UUID {
				id := uuid.New()
				ast.mu.Lock()
				defer ast.mu.Unlock()
				switch e {
				case entityWorkspace:
					ast.workspaces[id] = types.Workspace{
						ID: id, Name: "ws-shape", OwnedBy: memberSub, Status: types.WorkspaceScanned,
						Sources: []types.WorkspaceSource{{Type: types.WorkspaceSourceTypeEphemeral, Target: "/home/agent/work"}},
					}
					return id
				}
				ast.runs[id] = types.AgentRun{ID: id, CreatedBy: memberSub, State: types.RunRunning, Agent: "claude-code"}
				_ = rs.SaveCast(context.Background(), id.String(), strings.NewReader(`{"version":2}`+"\n"))
				if e == entityApproval {
					return aap.seed(id)
				}
				return id
			}
			reached := func(w int) bool {
				return w != http.StatusUnauthorized && w != http.StatusForbidden && w != http.StatusNotFound
			}

			for key, rc := range routeMatrix {
				method, pattern, _ := strings.Cut(key, " ")
				body := bodyFor(method, rc)
				switch rc.class {
				case classAdmin, classSecurity:
					t.Run(key, func(t *testing.T) {
						p := buildPath(pattern, "x1")
						assertNotBlocked(t, "admin", doSSO(t, srv, method, p, admin, body))
						w := doSSO(t, srv, method, p, sec, body)
						if rc.class == classSecurity {
							assertNotBlocked(t, "security_admin", w)
						} else if w.Code != http.StatusForbidden {
							t.Errorf("security_admin on a super route: %d, want 403", w.Code)
						}
						for name, c := range map[string]*http.Cookie{"member": member, "member2": member2} {
							if w := doSSO(t, srv, method, p, c, body); w.Code != http.StatusForbidden {
								t.Errorf("%s: %d, want 403; body=%s", name, w.Code, w.Body.String())
							}
						}
					})
				case classMember:
					t.Run(key, func(t *testing.T) {
						p := buildPath(pattern, uuid.New().String())
						for _, pr := range personas {
							assertNotBlocked(t, pr.name, doSSO(t, srv, method, p, pr.cookie, body))
						}
					})
				case classOwner:
					t.Run(key, func(t *testing.T) {
						if w := doSSO(t, srv, method, buildPath(pattern, owned(rc.entity).String()), member, body); !reached(w.Code) {
							t.Errorf("owning member: %d, want the handler's own answer; body=%s", w.Code, w.Body.String())
						}
						// Cross-member isolation: the existence-oracle 404, never 403.
						if w := doSSO(t, srv, method, buildPath(pattern, owned(rc.entity).String()), member2, body); w.Code != http.StatusNotFound {
							t.Errorf("member2 on member's entity: %d, want 404; body=%s", w.Code, w.Body.String())
						}
						if w := doSSO(t, srv, method, buildPath(pattern, owned(rc.entity).String()), admin, body); !reached(w.Code) {
							t.Errorf("admin on a foreign entity: %d, want the handler's own answer; body=%s", w.Code, w.Body.String())
						}
						w := doSSO(t, srv, method, buildPath(pattern, owned(rc.entity).String()), sec, body)
						if rc.ownerTier == tierSuper && w.Code != http.StatusNotFound {
							t.Errorf("security_admin on a foreign tierSuper entity: %d, want 404", w.Code)
						} else if rc.ownerTier == tierSecurity && !reached(w.Code) {
							t.Errorf("security_admin on a foreign tierSecurity entity: %d, want the handler's own answer", w.Code)
						}
					})
				}
			}
		})
	}
}
