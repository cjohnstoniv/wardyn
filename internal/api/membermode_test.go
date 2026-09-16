// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The server half of "view as member" (v0.7.4, field-report P2): the toggle
// route, the two mint doors it closes, the audit rows it writes, and — the one
// test that proves the mode is real rather than cosmetic — the whole gated-route
// walk driven by an ADMIN cookie carrying the flag.

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
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
		V: oidc.SessionCodecVersion, Sub: sub, Email: email, Role: role,
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
		w := do(t, srv, http.MethodPost, "/api/v1/me/member-mode", adminToken, `{"enabled":true}`)
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
		w := do(t, srv, http.MethodPost, "/api/v1/me/member-mode", "", `{"enabled":true}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
		}
	})

	t.Run("OIDC not configured at all", func(t *testing.T) {
		h := newHarness(t)
		srv := New(baseTestConfig(h, rbacStore{})) // cfg.OIDC == nil
		w := do(t, srv, http.MethodPost, "/api/v1/me/member-mode", adminToken, `{"enabled":true}`)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400 (never a nil-OIDC panic): %s", w.Code, w.Body.String())
		}
	})
}

// TestMemberMode_Toggle: the ordinary round trip. On clamps /me, off restores
// it, an empty body decodes to enabled:false and answers 200 (which is what
// routeMatrix's bodyFor sends), and a REAL member toggling on is a no-op 200.
func TestMemberMode_Toggle(t *testing.T) {
	srv, _ := memberModeServer(t)
	admin := ssoSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin)

	if body := meBody(t, srv, admin); body["member_mode"] != false || body["operator"] != true {
		t.Fatalf("before the toggle: member_mode=%v operator=%v, want false/true", body["member_mode"], body["operator"])
	}

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/member-mode", admin, `{"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST on = %d: %s", w.Code, w.Body.String())
	}
	on := sessionCookieFrom(t, w.Result().Cookies())

	body := meBody(t, srv, on)
	if body["member_mode"] != true {
		t.Errorf("member_mode = %v, want true", body["member_mode"])
	}
	if body["operator"] != false || body["security_operator"] != false {
		t.Errorf("operator/security_operator = %v/%v, want false/false", body["operator"], body["security_operator"])
	}
	if body["role"] != oidc.RoleMember {
		t.Errorf("role = %v, want %q", body["role"], oidc.RoleMember)
	}
	if body["principal"] != memberModeAdminSub {
		t.Errorf("principal = %v, want %q — the mode never changes who you are", body["principal"], memberModeAdminSub)
	}

	// EMPTY BODY: decodes to enabled:false, answers 200 — the shape the authz
	// matrix probes every classMember route with.
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/member-mode", on, "")
	if w.Code != http.StatusOK {
		t.Fatalf("POST with an empty body = %d, want 200: %s", w.Code, w.Body.String())
	}
	off := sessionCookieFrom(t, w.Result().Cookies())
	if body := meBody(t, srv, off); body["member_mode"] != false || body["operator"] != true {
		t.Fatalf("after the empty-body toggle: member_mode=%v operator=%v, want false/true", body["member_mode"], body["operator"])
	}

	// A REAL member toggling ON: a no-op 200, not a refusal — the route is
	// classMember precisely so that toggling OFF is always reachable, and a
	// member who finds the control must not meet a 4xx for asking to be what
	// they already are.
	member := ssoSession(t, "sub-real-member", "member@corp.example", oidc.RoleMember)
	w = doSSO(t, srv, http.MethodPost, "/api/v1/me/member-mode", member, `{"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("a real member toggling on = %d, want 200: %s", w.Code, w.Body.String())
	}
}

// TestMemberMode_DeniedOnEveryOperatorOnlyRoute is the test that proves the
// mode is REAL and not cosmetic: rbac_test.go's gatedRoutes walk, driven by an
// ADMIN session carrying the flag, must refuse exactly as it refuses a member —
// same 403, same body, same "the allowlist is never named" rule.
func TestMemberMode_DeniedOnEveryOperatorOnlyRoute(t *testing.T) {
	srv, _ := memberModeServer(t)
	admin := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)
	for _, rt := range gatedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, admin, "{}")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (member mode must refuse every gated route): %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "requires admin role") {
				t.Errorf("body = %q, want the admin-role message", w.Body.String())
			}
		})
	}
}

// TestMemberMode_AuditRowsNameTheAdmin: both rows the mode writes are
// attributed to the admin's OWN sub. An impersonation feature whose audit trail
// said "member" would be the thing the product spent six guards not building.
func TestMemberMode_AuditRowsNameTheAdmin(t *testing.T) {
	srv, h := memberModeServer(t)
	admin := ssoSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin)

	w := doSSO(t, srv, http.MethodPost, "/api/v1/me/member-mode", admin, `{"enabled":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("POST on = %d: %s", w.Code, w.Body.String())
	}
	ev := lastAuditEvent(t, h.audit.events, "auth.member_mode")
	if ev.Actor != memberModeAdminSub {
		t.Errorf("auth.member_mode actor = %q, want %q", ev.Actor, memberModeAdminSub)
	}
	if ev.Outcome != "success" {
		t.Errorf("auth.member_mode outcome = %q, want success", ev.Outcome)
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("decode auth.member_mode data: %v", err)
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
	if ddata["member_mode"] != true {
		t.Errorf("authz.denied data.member_mode = %v, want true", ddata["member_mode"])
	}
	if ddata["reason"] != "admin_surface" {
		t.Errorf("authz.denied reason = %v, want admin_surface", ddata["reason"])
	}

	// An ORDINARY member's denial carries no member_mode key at all — the flag
	// is a marker, not a field every row now has to answer.
	member := ssoSession(t, "sub-plain-member", "plain@corp.example", oidc.RoleMember)
	if d := doSSO(t, srv, http.MethodPost, "/api/v1/policies", member, "{}"); d.Code != http.StatusForbidden {
		t.Fatalf("POST /policies as a member = %d, want 403", d.Code)
	}
	den = lastAuditEvent(t, h.audit.events, "authz.denied")
	ddata = nil
	if err := json.Unmarshal(den.Data, &ddata); err != nil {
		t.Fatalf("decode authz.denied data: %v", err)
	}
	if _, present := ddata["member_mode"]; present {
		t.Errorf("a plain member's authz.denied carries member_mode: %#v", ddata)
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
	if ddata["reason"] != "security_admin_surface" || ddata["member_mode"] != true {
		t.Errorf("authz.denied data = %#v, want security_admin_surface + member_mode:true", ddata)
	}
}

// TestMemberMode_RefusesTokenAndKeyMint: the two doors that must REFUSE rather
// than clamp. OnLogin re-stamps every API token and SSH key of a principal to
// their freshly derived role at the next sign-in (store.RefreshAPITokenRoles,
// the ssh-key stamp refresh), so a credential minted "as a member" would
// silently become admin — a credential that outlives the mode is the one thing
// this feature must not leave behind.
func TestMemberMode_RefusesTokenAndKeyMint(t *testing.T) {
	srv, _ := memberModeServer(t)
	on := memberModeSSOSession(t, memberModeAdminSub, memberModeAdminEmail, oidc.RoleAdmin, true)

	for _, tc := range []struct{ name, path, body string }{
		{"api token", "/api/v1/me/tokens", `{"name":"minted-in-member-mode"}`},
		{"ssh key", "/api/v1/me/ssh-keys", `{"public_key":"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIIbdmX7dazl2gKzTjv0xfDCp4wTapmeoUaItm/kYMQz9 who@host"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodPost, tc.path, on, tc.body)
			if w.Code != http.StatusConflict {
				t.Fatalf("status = %d, want 409: %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "Exit member mode") {
				t.Errorf("body = %q, want the exit-member-mode refusal", w.Body.String())
			}
		})
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
