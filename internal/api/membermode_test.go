// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The server half of "view as member": the toggle
// route, the two mint doors it closes, the audit rows it writes, and — the one
// test that proves the mode is real rather than cosmetic — the whole gated-route
// walk driven by an ADMIN cookie carrying the flag.

import (
	"bytes"
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

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runner"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	memberModeAdminSub   = "sub-mm-admin"
	memberModeAdminEmail = "mm-admin@corp.example"
)

// memberModeSSOSession is ssoSession (rbac_test.go) with the member-mode flag —
// the same hand-rolled payload a ZERO-VALUE *oidc.Authenticator accepts, so the
// walk below drives the REAL cookie branch without standing up an IdP and
// without POSTing the toggle first.
func memberModeSSOSession(t *testing.T, sub, email, role string, mm bool) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{
		V: oidc.SessionCodecVersion, Sub: sub, Email: email, Role: role, UserType: "standard",
		MemberMode: mm,
		Expiry:     time.Now().UTC().Add(time.Hour),
	})
	if err != nil {
		t.Fatalf("marshal session: %v", err)
	}
	mac := hmac.New(sha256.New, nil)
	mac.Write(payload)
	return &http.Cookie{
		Name:  "wardyn_session",
		Value: base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)),
	}
}

// memberModeServer is rbacServer plus the harness, because these tests read the
// audit rows back. Secrets/OIDC are wired for the same reason rbacServer wires
// them: the SSO branch of humanOrAdminAuth has to be live.
func memberModeServer(t *testing.T) (*Server, *harness) {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = h.approvals
	return New(cfg), h
}

// meBody drives GET /me with the given cookie and returns the decoded body.
func meBody(t *testing.T, srv *Server, cookie *http.Cookie) map[string]any {
	t.Helper()
	w := doSSO(t, srv, http.MethodGet, "/api/v1/me", cookie, "")
	if w.Code != http.StatusOK {
		t.Fatalf("GET /me = %d: %s", w.Code, w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode /me: %v", err)
	}
	return body
}

// TestMemberMode_RefusesAdminTokenAndLocalMode: the no-human lane. An admin
// token, local mode and a deployment with no IdP at all are one shared
// credential with no per-person role to pause — isOperator would go on
// answering true, which is exactly the contradiction the mode must not ship.
// The guard runs FIRST, before s.cfg.OIDC is touched (it is nil in the third
// case).
func TestMemberMode_RefusesAdminTokenAndLocalMode(t *testing.T) {
	t.Run("admin token", func(t *testing.T) {
		srv, _ := memberModeServer(t)
		w := do(t, srv, http.MethodPost, "/api/v1/me/view", adminToken, `{"view":"user"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "signed-in SSO human") {
			t.Errorf("body = %q, want the no-human refusal", w.Body.String())
		}
	})

	t.Run("local mode", func(t *testing.T) {
		h := newHarness(t)
		cfg := baseTestConfig(h, rbacStore{})
		cfg.LocalMode = true
		srv := New(cfg)
		w := do(t, srv, http.MethodPost, "/api/v1/me/view", "", `{"view":"user"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("OIDC not configured at all", func(t *testing.T) {
		h := newHarness(t)
		srv := New(baseTestConfig(h, rbacStore{})) // cfg.OIDC == nil
		w := do(t, srv, http.MethodPost, "/api/v1/me/view", adminToken, `{"view":"user"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (never a nil-OIDC panic): %s", w.Code, w.Body.String())
		}
	})

	// R-01. The `wdn_` token lane publishes a human identity through the SAME
	// withHumanIdentity the SSO branch uses, and http.go mounts that lane
	// REGARDLESS of OIDC — deliberately, so an operator who never configured SSO
	// can still hold a personal token. So `oidcHumanFromContext(ctx) != ""` with
	// a nil *Authenticator is reachable, and a token caller who also attaches
	// ANY wardyn_session cookie reached sessionHMAC on it: a nil deref, caught by
	// chi's Recoverer as a 500 plus a stack dump per request, on a door any
	// token holder can knock on. The guard is TWO conditions for this reason.
	t.Run("a wdn_ token on an OIDC-less deployment, with a forged cookie", func(t *testing.T) {
		h := newHarness(t)
		st := newTokenMemStore()
		srv := New(baseTestConfig(h, st)) // cfg.OIDC == nil, but the token lane is mounted
		const raw = "wdn_membermode_probe"
		if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
			ID: uuid.New(), Principal: "sub-token-holder", Email: "holder@corp.example",
			Role: oidc.RoleAdmin, Name: raw, CreatedAt: time.Now().UTC().Add(-time.Hour),
		}, raw); err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/v1/me/view", strings.NewReader(`{"view":"user"}`))
		r.Header.Set("Authorization", "Bearer "+raw)
		// The trivially-forged cookie: decodeSession gets PAST its absent-cookie
		// early return and reaches the HMAC.
		r.AddCookie(&http.Cookie{Name: "wardyn_session", Value: "AA.AA"})
		r.Host = "127.0.0.1"
		r.RemoteAddr = "127.0.0.1:54321"
		w := httptest.NewRecorder()
		panicFails(t, srv.Handler()).ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 — a 500 here is the nil-Authenticator deref: %s", w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "signed-in SSO human") {
			t.Errorf("body = %q, want the no-human refusal", w.Body.String())
		}
	})
}

// TestMemberMode_Toggle: the ordinary round trip. On clamps /me, off restores
// it, an empty body decodes to the zero View (off) and answers 200 (which is what
// routeMatrix's bodyFor sends), and a REAL member toggling on is a no-op 200.
func TestMemberMode_Toggle(t *testing.T) {
	srv, _ := memberModeServer(t)
	admin := ssoSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin)

	if body := meBody(t, srv, admin); body["user_view"] != false || body["operator"] != true {
		t.Fatalf("before the toggle: user_view=%v operator=%v, want false/true", body["user_view"], body["operator"])
	}

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST on = %d: %s", w.Code, w.Body.String())
	}
	on := sessionCookieFrom(t, w.Result().Cookies())

	body := meBody(t, srv, on)
	if body["user_view"] != true {
		t.Errorf("user_view = %v, want true", body["user_view"])
	}
	if body["operator"] != false || body["security_operator"] != false {
		t.Errorf("operator/security_operator = %v/%v, want false/false", body["operator"], body["security_operator"])
	}
	if body["role"] != oidc.RoleUser {
		t.Errorf("role = %v, want %q", body["role"], oidc.RoleUser)
	}
	if body["principal"] != memberModeAdminSub {
		t.Errorf("principal = %v, want %q — the mode never changes who you are", body["principal"], memberModeAdminSub)
	}

	// EMPTY BODY: decodes to the zero View (off), answers 200 — the shape the authz
	// matrix probes every classMember route with.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", on, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST with an empty body = %d, want 200: %s", w.Code, w.Body.String())
	}
	off := sessionCookieFrom(t, w.Result().Cookies())
	if body := meBody(t, srv, off); body["user_view"] != false || body["operator"] != true {
		t.Fatalf("after the empty-body toggle: user_view=%v operator=%v, want false/true", body["user_view"], body["operator"])
	}

	// A REAL member toggling ON: a no-op 200, not a refusal — the route is
	// classMember precisely so that toggling OFF is always reachable, and a
	// member who finds the control must not meet a 4xx for asking to be what
	// they already are.
	member := ssoSession(t, "sub-real-member", "member@corp.example", oidc.RoleUser)
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/view", member, `{"view":"user"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a real member toggling on = %d, want 200: %s", w.Code, w.Body.String())
	}

	// R-03. A CHUNKED body reports ContentLength == -1, so a `> 0` optional-body
	// guard skips the decode entirely and answers 200 {"user_view":false} to a
	// request that asked to ENTER the mode — the admin stays admin and the only
	// hint is the absent banner.
	t.Run("a chunked body is decoded, not discarded", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/me/view", strings.NewReader(`{"view":"user"}`))
		r.ContentLength = -1
		r.AddCookie(admin)
		cw := httptest.NewRecorder()
		panicFails(t, srv.Handler()).ServeHTTP(cw, r)
		if cw.Code != http.StatusOK {
			t.Fatalf("chunked POST = %d, want 200: %s", cw.Code, cw.Body.String())
		}
		if b := meBody(t, srv, sessionCookieFrom(t, cw.Result().Cookies())); b["user_view"] != true {
			t.Errorf("user_view = %v after a chunked {\"view\":\"user\"}, want true", b["user_view"])
		}
	})
}

// TestMemberMode_DeniedOnEveryOperatorOnlyRoute is the test that proves the
// mode is REAL and not cosmetic: every admin-tier route, driven by an ADMIN
// session carrying the flag, must refuse exactly as it refuses a member.
//
// It walks authz_test.go's routeMatrix — BOTH admin classes — rather than only
// rbac_test.go's gatedRoutes table, and the difference is the point (R-06).
// gatedRoutes is a hand-maintained list of the ~22 widest operatorOnly routes;
// routeMatrix is the COMPLETENESS-CHECKED table (TestAuthzMatrix walks chi and
// fails the build on an unclassified route), and it is the only one of the two
// that covers classSecurity — so a future securityOps route mounted on the wrong
// group, or a new admin route, is caught here without anybody remembering to add
// it. The gatedRoutes walk is kept as a subtest because it costs one loop and it
// probes with the seeded fixtures that table's server was built for.
//
// The 403 body is asserted BYTE-IDENTICAL to what a real member session gets for
// the same route, not merely as "contains the admin-role sentence": "a member
// mode admin is refused the way a member is" is the claim, and a string match
// against a literal both requireOperator and requireSecurityOperator write
// cannot tell the two tiers' answers apart at all.
func TestMemberMode_DeniedOnEveryOperatorOnlyRoute(t *testing.T) {
	// The MAXIMALLY-CONFIGURED server the matrix itself is built from (Secrets,
	// RecordingStore, SessionRevocations all wired) — anything less 404s the
	// conditionally-mounted admin routes and the walk would read those as
	// "refused" when they were merely absent. Same reason csrf_test.go's fence
	// uses it.
	matrixSrv, _, _, _ := newAuthzMatrixServer(t)
	mmAdmin := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)
	realMember := ssoSession(t, "sub-control-member", "control@corp.example", oidc.RoleUser)

	adminTierRoutes := 0
	for key, rc := range routeMatrix {
		if rc.class != classAdmin && rc.class != classSecurity {
			continue
		}
		method, pattern, ok := strings.Cut(key, " ")
		if !ok {
			t.Fatalf("malformed routeMatrix key %q", key)
		}
		// GET /metrics is classAdmin but lives on the TOP-LEVEL router outside
		// /api/v1 with its own gate; every other admin-tier row is an API route.
		if !strings.HasPrefix(pattern, "/api/v1/") {
			continue
		}
		adminTierRoutes++
		t.Run(key, func(t *testing.T) {
			path := buildPath(pattern, "x1")
			body := bodyFor(method, rc)
			got := doSSO(t, matrixSrv, method, path, mmAdmin, body)
			want := doSSO(t, matrixSrv, method, path, realMember, body)
			if got.Code != http.StatusForbidden {
				t.Fatalf("member-mode admin: status = %d, want 403: %s", got.Code, got.Body.String())
			}
			if want.Code != http.StatusForbidden {
				t.Fatalf("control: a real member got %d on this route, so it is not admin-tier here: %s", want.Code, want.Body.String())
			}
			if got.Body.String() != want.Body.String() {
				t.Errorf("body = %q, want byte-identical to a real member's %q", got.Body.String(), want.Body.String())
			}
		})
	}
	// A matrix that stopped yielding admin-tier rows would make this test
	// vacuously green — the exact failure the walk exists to prevent
	// (csrf_test.go's own fence guards itself the same way).
	if adminTierRoutes < 40 {
		t.Fatalf("walked only %d admin-tier API routes; routeMatrix carries ~50 — something stopped enumerating", adminTierRoutes)
	}

	t.Run("gatedRoutes", func(t *testing.T) {
		srv, _ := memberModeServer(t) // the fixtures that table's own server is built for
		for _, rt := range gatedRoutes {
			t.Run(rt.method+" "+rt.path, func(t *testing.T) {
				w := doSSO(t, srv, rt.method, rt.path, mmAdmin, "{}")
				if w.Code != http.StatusForbidden {
					t.Fatalf("status = %d, want 403 (member mode must refuse every gated route): %s", w.Code, w.Body.String())
				}
				if !strings.Contains(w.Body.String(), "requires admin role") {
					t.Errorf("body = %q, want the admin-role message", w.Body.String())
				}
			})
		}
	})
}

// TestMemberMode_AuditRowsNameTheAdmin: both rows the mode writes are
// attributed to the admin's OWN sub. An impersonation feature whose audit trail
// said "member" would be the thing the product spent six guards not building.
func TestMemberMode_AuditRowsNameTheAdmin(t *testing.T) {
	// ownerHarness rather than memberModeServer: the two IN-HANDLER
	// admin_surface twins asserted below need a real operator-owned workspace
	// row and the secret store mounted, and this is the harness the ownership
	// tests already build for exactly that.
	srv, st, h := ownerHarness(t, runner.UserMountPolicy{})
	memberModeOperatorWS := st.put(types.Workspace{}).String() // owned_by == "" — operator-owned
	admin := ssoSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST on = %d: %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.events, "auth.user_view.set")
	if ev.Actor != memberModeAdminSub {
		t.Errorf("auth.user_view.set actor = %q, want %q", ev.Actor, memberModeAdminSub)
	}
	if ev.Outcome != "success" {
		t.Errorf("auth.user_view.set outcome = %q, want success", ev.Outcome)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode auth.user_view.set data: %v", err)
	}
	if data["enabled"] != true {
		t.Errorf("data.enabled = %v, want true", data["enabled"])
	}
	if data["real_role"] != oidc.RoleAdmin {
		t.Errorf("data.real_role = %v, want %q — the row records what is being paused", data["real_role"], oidc.RoleAdmin)
	}

	// The refusal met IN the mode is self-labelling, so a denial stream can be
	// read as "an admin exercising the member path" rather than an incident.
	on := sessionCookieFrom(t, w.Result().Cookies())
	if d := doSSO(t, srv, http.MethodPost, "/api/v1/policies", on, "{}"); d.Code != http.StatusForbidden {
		t.Fatalf("POST /policies in member mode = %d, want 403", d.Code)
	}
	den := lastAuditEvent(t, h.audit.events, "authz.denied")
	if den.Actor != memberModeAdminSub {
		t.Errorf("authz.denied actor = %q, want %q", den.Actor, memberModeAdminSub)
	}
	var ddata map[string]any
	if err := json.Unmarshal(den.Data, &ddata); err != nil {
		t.Fatalf("decode authz.denied data: %v", err)
	}
	if ddata["user_view"] != true {
		t.Errorf("authz.denied data.user_view = %v, want true", ddata["user_view"])
	}
	if ddata["reason"] != "admin_surface" {
		t.Errorf("authz.denied reason = %v, want admin_surface", ddata["reason"])
	}

	// R-02. The marker must ride EVERY admin-tier refusal, not only the two
	// middleware chokepoints: two in-handler twins emit the identical
	// admin_surface datum, and secrets.go says so in a shape-identity comment.
	// A marker present at two of four sites is unreliable for the one reader it
	// was added for — an operator filtering the denial stream.
	for _, tc := range []struct {
		name, method, path string
	}{
		// DELETE /workspaces/{id} is classOwner, so it passes the middleware and
		// the refusal is raised INSIDE the handler by getWorkspaceAuthorized —
		// which is the point. (PUT /requirements would 403 at requireOperator
		// and prove nothing about the twin.)
		{"getWorkspaceAuthorized (operator-owned workspace)", http.MethodDelete, "/api/v1/workspaces/" + memberModeOperatorWS},
		{"secrets ?owner= gate", http.MethodGet, "/api/v1/secrets?owner=somebody-else"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if d := doSSO(t, srv, tc.method, tc.path, on, "{}"); d.Code != http.StatusForbidden {
				t.Fatalf("%s in member mode = %d, want 403: %s", tc.name, d.Code, d.Body.String())
			}
			ev := lastAuditEvent(t, h.audit.events, "authz.denied")
			var data map[string]any
			if err := json.Unmarshal(ev.Data, &data); err != nil {
				t.Fatalf("decode authz.denied data: %v", err)
			}
			if data["reason"] != "admin_surface" {
				t.Fatalf("reason = %v, want admin_surface (wrong row matched)", data["reason"])
			}
			if data["user_view"] != true {
				t.Errorf("authz.denied data = %#v, want user_view:true", data)
			}
			if ev.Actor != memberModeAdminSub {
				t.Errorf("actor = %q, want %q", ev.Actor, memberModeAdminSub)
			}
		})
	}

	// An ORDINARY member's denial carries no user_view key at all — the flag
	// is a marker, not a field every row now has to answer.
	member := ssoSession(t, "sub-plain-member", "plain@corp.example", oidc.RoleUser)
	if d := doSSO(t, srv, http.MethodPost, "/api/v1/policies", member, "{}"); d.Code != http.StatusForbidden {
		t.Fatalf("POST /policies as a member = %d, want 403", d.Code)
	}
	den = lastAuditEvent(t, h.audit.events, "authz.denied")
	ddata = nil
	if err := json.Unmarshal(den.Data, &ddata); err != nil {
		t.Fatalf("decode authz.denied data: %v", err)
	}
	if _, present := ddata["user_view"]; present {
		t.Errorf("a plain member's authz.denied carries user_view: %#v", ddata)
	}
}

// TestMemberMode_SecurityAdminSurfaceDeniedToo: the security-governance tier's
// own refusal is clamped by the same single origin, and it labels itself the
// same way.
func TestMemberMode_SecurityAdminSurfaceDeniedToo(t *testing.T) {
	srv, h := memberModeServer(t)
	admin := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)
	w := doSSO(t, srv, http.MethodGet, "/api/v1/permissions", admin, "")
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET /permissions in member mode = %d, want 403: %s", w.Code, w.Body.String())
	}
	den := lastAuditEvent(t, h.audit.events, "authz.denied")
	var ddata map[string]any
	if err := json.Unmarshal(den.Data, &ddata); err != nil {
		t.Fatalf("decode authz.denied data: %v", err)
	}
	if ddata["reason"] != "security_admin_surface" || ddata["user_view"] != true {
		t.Errorf("authz.denied data = %#v, want security_admin_surface + user_view:true", ddata)
	}
}

// TestMemberMode_RefusesTokenMint: the door that must REFUSE rather than
// clamp. OnLogin re-stamps every API token of a principal to their freshly
// derived role at the next sign-in (store.RefreshAPITokenIdentity), so a token
// minted "as a member" would silently become admin — a credential that outlives
// the mode is the one thing this feature must not leave behind. The SSH-key
// door stores a capped key instead (TestSSHKeys_UserViewAddIsCapped).
func TestMemberMode_RefusesTokenMint(t *testing.T) {
	srv, _ := memberModeServer(t)
	on := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)

	for _, tc := range []struct{ name, path, body string }{
		{"api token", "/api/v1/me/tokens", `{"name":"minted-in-member-mode"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodPost, tc.path, on, tc.body)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "Exit the user view") {
				t.Errorf("body = %q, want the exit-member-mode refusal", w.Body.String())
			}
		})
	}
}

// TestMemberMode_ExistingAPITokenKeepsItsOwnRole is ceiling 4, made
// executable. The mode is per-session: a `wdn_` token the human already holds
// replays its own DB-stamped role (apitokens.go), so it still reaches
// operator-only routes while the very same human's browser cookie is clamped.
// The 409 mint doors stop NEW credentials; they cannot reach into old ones.
//
// It also pins against the tempting wrong fix: teaching the token lane to read
// the session would clamp a CLI caller by the state of an unrelated browser.
func TestMemberMode_ExistingAPITokenKeepsItsOwnRole(t *testing.T) {
	h := newHarness(t)
	st := newTokenMemStore()
	cfg := baseTestConfig(h, st)
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)

	const raw = "wdn_minted_before_the_mode"
	if _, err := st.CreateAPIToken(context.Background(), types.APIToken{
		ID: uuid.New(), Principal: memberModeAdminSub, Email: memberModeAdminEmail,
		Role: oidc.RoleAdmin, Name: raw, CreatedAt: time.Now().UTC().Add(-time.Hour),
	}, raw); err != nil {
		t.Fatal(err)
	}

	// The same human's COOKIE is in the mode and is refused.
	on := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)
	if w := doSSO(t, srv, http.MethodPost, "/api/v1/policies", on, "{}"); w.Code != http.StatusForbidden {
		t.Fatalf("the cookie lane = %d, want 403 (the mode is on): %s", w.Code, w.Body.String())
	}
	// Their TOKEN is not.
	w := do(t, srv, http.MethodPost, "/api/v1/policies", raw, "{}")
	if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
		t.Fatalf("the token lane = %d, want the handler to run — a wdn_ token keeps its own stamped role: %s", w.Code, w.Body.String())
	}
}

// sessionCookieFrom picks the re-issued wardyn_session out of a response.
func sessionCookieFrom(t *testing.T, cookies []*http.Cookie) *http.Cookie {
	t.Helper()
	for _, c := range cookies {
		if c.Name == "wardyn_session" {
			return c
		}
	}
	t.Fatal("no wardyn_session cookie on the response")
	return nil
}

// TestMemberMode_InHandlerAdminTierRefusalsCarryTheMarker pins the marker on the
// two in-handler admin-tier refusals.
//
// TestMemberMode_AuditRowsNameTheAdmin above pins four sites, and the doc
// comment on internal/authz.Datum names those four as "every admin-tier
// refusal". Two more emitters are pinned here: resolveAlwaysTarget's rule 6
// (`always` is security-admin-only) and workspaces.go's `workspaces.llm_cred`
// arm (`admin_surface`). Both are reachable INSIDE the view by an admin doing
// exactly what the member Getting Started card invites — deciding their own
// run's held egress, creating a workspace — so a reviewer filtering the denial
// stream must not read an admin's own user walk as a member incident, the one
// outcome the marker exists to prevent.
//
// `method` rides along for the same reason: a marker on a row whose shape
// differs from the middleware's is still a row the same filter cannot group.
func TestMemberMode_InHandlerAdminTierRefusalsCarryTheMarker(t *testing.T) {
	// resolveAlwaysTarget's rule 6. The session's sub IS the fixture run's
	// CreatedBy, so the ownership gate ahead of rule 6 passes and the refusal
	// under test is the one that fires — an admin in member mode picking
	// `always` on THEIR OWN run's approval, which is precisely the scope the
	// member Getting Started's approvals hint advertises.
	t.Run("decision_scope always", func(t *testing.T) {
		f := newScopeFixture(t)
		id := f.seedEgress(t, "registry.npmjs.org")
		on := memberModeSSOSession(t, f.memberID, memberModeAdminEmail, oidc.RoleAdmin, true)
		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve", on,
			decideBody(t, types.ScopeAlways, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("member-mode admin picking always = %d, want 403: %s", w.Code, w.Body.String())
		}
		rows := denialRows(f.rec.events)
		if len(rows) != 1 {
			t.Fatalf("authz.denied rows = %d, want 1", len(rows))
		}
		data := auditData(t, rows[0])
		if data["reason"] != "security_admin_surface" {
			t.Errorf("reason = %v, want security_admin_surface", data["reason"])
		}
		if data["user_view"] != true {
			t.Errorf("data = %#v, want user_view:true — an admin walking the member path", data)
		}
		if data["method"] != http.MethodPost {
			t.Errorf("method = %v, want POST — the row is shape-identical to the middleware's", data["method"])
		}
	})

	// refuse, reached from handleCreateWorkspace's llm_cred arm:
	// secretOwnerFromRequest returns the caller's principal once the role is
	// clamped, so the member arm fires for a member-mode admin.
	t.Run("workspace create llm_cred", func(t *testing.T) {
		srv, _, h := ownerHarness(t, runner.UserMountPolicy{})
		on := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)
		w := doSSO(t, srv, http.MethodPost, "/api/v1/workspaces", on,
			`{"name":"mine","llm_cred":{"integration_ref":"corp-openai"}}`)
		if w.Code != http.StatusForbidden {
			t.Fatalf("member-mode admin create with llm_cred = %d, want 403: %s", w.Code, w.Body.String())
		}
		data := auditData(t, lastAuditEvent(t, h.audit.events, "authz.denied"))
		if data["reason"] != "admin_surface" {
			t.Errorf("reason = %v, want admin_surface", data["reason"])
		}
		if data["user_view"] != true {
			t.Errorf("data = %#v, want user_view:true", data)
		}
		if data["method"] != http.MethodPost {
			t.Errorf("method = %v, want POST", data["method"])
		}
	})

	// THE CONTROL: a REAL member meeting the same two refusals carries no
	// marker at all. The field is a marker, not one every row answers.
	t.Run("a real member carries no marker", func(t *testing.T) {
		f := newScopeFixture(t)
		id := f.seedEgress(t, "registry.npmjs.org")
		w := doSSO(t, f.srv, http.MethodPost, "/api/v1/approvals/"+id.String()+"/approve",
			ssoSession(t, f.memberID, "member@corp.example", oidc.RoleUser),
			decideBody(t, types.ScopeAlways, nil))
		if w.Code != http.StatusForbidden {
			t.Fatalf("member picking always = %d, want 403: %s", w.Code, w.Body.String())
		}
		rows := denialRows(f.rec.events)
		if len(rows) != 1 {
			t.Fatalf("authz.denied rows = %d, want 1", len(rows))
		}
		if v, present := auditData(t, rows[0])["user_view"]; present {
			t.Errorf("a plain member's row carries user_view=%v", v)
		}
	})
}

// TestMemberMode_RealMemberTogglingOnChangesNothing is other half, at
// the layer the defect is actually felt: the mint doors and /me.
//
// handleSetUserView's only guard is "is there an SSO human", and its comment
// calls a real member toggling ON a no-op. It was not one — the flag landed on
// the member's cookie, so GET /me answered user_view:true (the console then
// paints a banner naming an admin role they do not hold) and POST /me/ssh-keys
// and POST /me/tokens both 409'd "Exit member mode…", breaking the member
// Getting Started's own "Connect your tools · Add SSH key" card and
// docs/USERS.md's SSH path. There is no UI affordance for this — the menu
// item is gated on operator||securityOperator — so the only way in is a
// hand-rolled POST, which is why it is LOW and not why it is acceptable.
func TestMemberMode_RealMemberTogglingOnChangesNothing(t *testing.T) {
	// newTokenMemStore, not memberModeServer's rbacStore: the mint door asserted
	// below has to be driven THROUGH into its handler, and rbacStore's
	// credential methods are unimplemented stubs that panic there — a recovered
	// 500 would pass a "not 409" assertion while proving nothing.
	h := newHarness(t)
	cfg := baseTestConfig(h, newTokenMemStore())
	cfg.OIDC = &oidc.Authenticator{}
	srv := New(cfg)
	member := ssoSession(t, "sub-w6-member", "w6-member@corp.example", oidc.RoleUser)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", member, `{"view":"user"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a real member toggling on = %d, want 200: %s", w.Code, w.Body.String())
	}
	// Whatever the server handed back is what the browser would be holding on
	// the next request, so the two assertions below are driven through it —
	// otherwise they would pass vacuously by re-using the cookie we sent.
	after := member
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" {
			t.Errorf("the response re-signed the member's session: %q — there is nothing to pause", c.Value)
			after = c
		}
	}
	if body := meBody(t, srv, after); body["user_view"] != false {
		t.Errorf("/me user_view = %v, want false — the member is already what they asked to be", body["user_view"])
	}
	// The doors that 409 INSIDE the mode stay open. A 409 on either would break
	// the member Getting Started's own "Connect your tools · Add SSH key" card
	// and docs/USERS.md's SSH path.
	tok := doSSO(t, srv, http.MethodPost, "/api/v1/me/tokens", after, `{"name":"after-the-no-op"}`)
	if tok.Code == http.StatusConflict {
		t.Errorf("POST /me/tokens = 409 for a member who never entered the mode: %s", tok.Body.String())
	}

	// The audit row is still written — the request WAS made and answered, and
	// real_role records the tier it was made from.
	ev := lastAuditEvent(t, h.audit.events, "auth.user_view.set")
	if data := auditData(t, ev); data["real_role"] != oidc.RoleUser {
		t.Errorf("real_role = %v, want %q", data["real_role"], oidc.RoleUser)
	}
}

// TestUserView_ViewFieldReplacesEnabled (#617) pins the 0.8 wire contract:
// POST /me/view takes `{"view":"user"|"admin"}`, never the 0.7 boolean
// `enabled` — a clean break, so an old caller still sending `enabled` is
// refused 400 (decodeStrict disallows the unknown field), and an unrecognised
// View value is refused 400 rather than silently defaulting.
func TestUserView_ViewFieldReplacesEnabled(t *testing.T) {
	srv, _ := memberModeServer(t)
	admin := ssoSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin)

	t.Run(`view:"user" turns it on`, func(t *testing.T) {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		on := sessionCookieFrom(t, w.Result().Cookies())
		if body := meBody(t, srv, on); body["user_view"] != true {
			t.Errorf("user_view = %v, want true", body["user_view"])
		}

		t.Run(`view:"admin" turns it back off`, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", on, `{"view":"admin"}`)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d: %s", w.Code, w.Body.String())
			}
			off := sessionCookieFrom(t, w.Result().Cookies())
			if body := meBody(t, srv, off); body["user_view"] != false {
				t.Errorf("user_view = %v, want false", body["user_view"])
			}
		})
	})

	t.Run("an unrecognised view value is refused 400", func(t *testing.T) {
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"member"}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("a pre-0.8 enabled:true body is refused, never silently honoured", func(t *testing.T) {
		// Clean break, no alias (docs/OPERATIONS.md's "Renamed in 0.8"
		// appendix): decodeStrict's DisallowUnknownFields refuses the old
		// `enabled` key outright (400) rather than decode it as View:"" and
		// silently answer 200 off — the fail-safe direction, same rule
		// userViewRequest's own doc comment states for a rolling upgrade.
		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"enabled":true}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 — a pre-0.8 body must never silently enter or skip the view: %s", w.Code, w.Body.String())
		}
	})
}

// countAuditEvents returns how many recorded events carry the given action —
// TestUserView_DualEmitsTheLegacyAuditAction uses it to pin "exactly one per
// toggle", which lastAuditEvent (LAST match) cannot see a double-emit past.
func countAuditEvents(events []types.AuditEvent, action string) int {
	n := 0
	for _, ev := range events {
		if ev.Action == action {
			n++
		}
	}
	return n
}

// TestUserView_DualEmitsTheLegacyAuditAction (#617, OD-18) pins the one-minor
// compat window: every toggle writes BOTH auth.user_view.set (the 0.8 name) and
// auth.member_mode (the pre-0.8 name it replaces), with byte-identical Data —
// checked as raw bytes, not a couple of hand-picked fields, so a compat row
// that silently lost a key (e.g. no_credential) fails this test — and each
// action exactly once, so a dashboard or SIEM rule still filtering on the old
// action name keeps seeing rows without double-counting them. Two toggles are
// covered: the plain one and the no-credential preview posture, since that
// second key is the one most likely to go missing from just one of the two
// rows. docs/AUDIT-ACTIONS.md and docs/OPERATIONS.md's "Renamed in 0.8"
// appendix both say the compat row is removed in 0.9 — this test is the one
// to delete then.
func TestUserView_DualEmitsTheLegacyAuditAction(t *testing.T) {
	assertDualEmit := func(t *testing.T, events []types.AuditEvent, wantNoCredential bool) {
		t.Helper()
		if got := countAuditEvents(events, "auth.user_view.set"); got != 1 {
			t.Errorf("auth.user_view.set count = %d, want exactly 1", got)
		}
		if got := countAuditEvents(events, "auth.member_mode"); got != 1 {
			t.Errorf("auth.member_mode count = %d, want exactly 1", got)
		}
		newRow := lastAuditEvent(t, events, "auth.user_view.set")
		oldRow := lastAuditEvent(t, events, "auth.member_mode")
		if newRow.Actor != memberModeAdminSub || oldRow.Actor != memberModeAdminSub {
			t.Errorf("actor = %q / %q, want %q on both", newRow.Actor, oldRow.Actor, memberModeAdminSub)
		}
		if !bytes.Equal(newRow.Data, oldRow.Data) {
			t.Errorf("Data = %s / %s, want byte-identical", newRow.Data, oldRow.Data)
		}
		if _, present := auditData(t, newRow)["no_credential"]; present != wantNoCredential {
			t.Errorf("no_credential present = %v, want %v", present, wantNoCredential)
		}
	}

	t.Run("plain", func(t *testing.T) {
		srv, h := memberModeServer(t)
		admin := ssoSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin)

		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user"}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		assertDualEmit(t, h.audit.events, false)
	})

	t.Run("no_credential preview", func(t *testing.T) {
		srv, audit, _, _ := memberPreviewSrv(t)
		admin := memberPreviewSessionAs(t, memberModeAdminSub, oidc.RoleAdmin, false, false)

		w := doSSO(t, srv, http.MethodPost, "/api/v1/me/view", admin, `{"view":"user","no_credential":true}`)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d: %s", w.Code, w.Body.String())
		}
		assertDualEmit(t, audit.rows, true)
	})
}
