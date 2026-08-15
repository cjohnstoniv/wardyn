// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

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
	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── the minimal viewer/operator role gate (requireOperator) ─────────────────

const (
	rbacOperator = "Ops@Corp.Example" // as configured: mixed case, on purpose
	rbacViewer   = "dev@corp.example"
)

// rbacStore serves the READ routes the viewer must keep. Every other store
// method is nil (embedded interface), which is fine: the gated write handlers
// all validate the body/params before they touch the store, so an operator
// request in these tests stops at a 4xx without ever dereferencing it.
type rbacStore struct{ store.Store }

func (rbacStore) ListPolicies(context.Context) ([]types.RunPolicy, error) {
	return nil, nil
}
func (rbacStore) ListWorkspaces(context.Context) ([]types.Workspace, error) {
	return nil, nil
}
func (rbacStore) GetSiteConfig(context.Context) (types.SiteConfig, error) {
	return types.SiteConfig{}, nil
}

// rbacServer builds a server with OIDC configured (so the SSO branch of
// humanOrAdminAuth is live), a secret store (so the secrets + harness-credential
// routes mount), the harness's approval service (so GET /approvals answers) and
// the given operator allowlist.
func rbacServer(t *testing.T, operatorEmails ...string) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.Secrets = getErrStore{getErr: secretstore.ErrNotFound}
	cfg.Approvals = h.approvals
	cfg.OperatorEmails = operatorEmails
	return New(cfg)
}

// ssoSession mints a wardyn_session cookie a ZERO-VALUE *oidc.Authenticator
// accepts — its HMAC key is the empty key — so these tests drive the REAL OIDC
// branch of humanOrAdminAuth end to end (router included) without standing up an
// IdP. The encoding is the one oidc.encodeSession produces:
// base64url(json(Session)) "." base64url(HMAC-SHA256(json)).
//
// role must be oidc.RoleAdmin or oidc.RoleMember — decodeSession treats an
// empty Role as no session (the pre-0.5-cookie guard), so every session this
// helper mints needs one explicitly. Since B2, role (not email-list
// membership) is what requireOperator/isOperator gate on (see TestIsOperator
// and TestRequireOperator_RoleGatesNotEmailList) — email still rides along for
// /me and audit attribution.
func ssoSession(t *testing.T, sub, email, role string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{Sub: sub, Email: email, Role: role, Expiry: time.Now().UTC().Add(time.Hour)})
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

// doSSO issues a request authenticated by an SSO session cookie (no bearer).
func doSSO(t *testing.T, srv *Server, method, path string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// operatorCtx models what humanOrAdminAuth publishes for a verified SSO human,
// role included (B2: isOperator now gates on it — see TestIsOperator).
func operatorCtx(sub, email, role string) context.Context {
	ctx := withOIDCEmail(withOIDCHuman(context.Background(), sub), email)
	return withOIDCRole(ctx, role)
}

// TestIsOperator covers the role resolution itself: an admin-role session is an
// operator, a member-role one is not, and a caller with no verified OIDC human
// at all (admin token, local mode) is always an operator — a single shared
// credential carries no per-human role to demote. The email/allowlist axis
// (WARDYN_OIDC_OPERATOR_EMAILS) is no longer consulted here at all — it feeds
// role DERIVATION upstream (oidc.deriveRole, tested in internal/auth/oidc), not
// this gate; see TestRequireOperator_RoleGatesNotEmailList for the pin.
func TestIsOperator(t *testing.T) {
	tests := []struct {
		name  string
		ctx   context.Context
		wantP bool
	}{
		{"admin token: no session, is operator", context.Background(), true},
		{"local mode: no session, is operator", withLocalPrincipal(context.Background(), "local:alice"), true},
		{"sso session, admin role, is operator", operatorCtx("sub-1", rbacOperator, oidc.RoleAdmin), true},
		{"sso session, member role, is not operator", operatorCtx("sub-2", rbacViewer, oidc.RoleMember), false},
		// Defense-in-depth: B1's decodeSession refuses to hand out a session with
		// an empty role at all, so this should be unreachable in practice — but
		// isOperator must still fail closed, not open, if it ever were.
		{"sso session, empty role, fails closed", operatorCtx("sub-3", rbacViewer, ""), false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{}
			if got := s.isOperator(tc.ctx); got != tc.wantP {
				t.Fatalf("isOperator = %v, want %v", got, tc.wantP)
			}
		})
	}
}

// gatedRoutes is the FULL set requireOperator gates, i.e. the admin half of the
// role split (member = read + launch/own runs). It is the test's copy of the
// intent, so adding a mutating route to one of these clusters without gating it
// fails here.
//
// NOT here: POST /runs/{id}/attach-ticket and POST /approvals/{id}/approve|deny.
// B2 moved both DOWN from admin-only to owner-or-admin (a member may act on a
// run/approval they own) — they no longer refuse EVERY member uniformly, so
// they don't fit this table's binary admin/not-admin shape. Their owner-vs-
// foreign-vs-admin behavior is covered by the chi.Walk-enumerated matrix in
// authz_test.go instead.
//
// Path ids are deliberately NON-UUID/short where the handler parses one: every
// gated handler validates its params before it touches a store, so an OPERATOR
// request stops at a 4xx instead of dereferencing rbacStore's nil embedded
// Store. That is what TestRequireOperator_OperatorPassesEveryGatedRoute asserts
// (anything but 401/403).
var gatedRoutes = []struct{ method, path string }{
	// 1. managed harness credential
	{http.MethodPost, "/api/v1/setup/harness-login"},
	{http.MethodPut, "/api/v1/setup/harness-credential/anthropic"},
	{http.MethodDelete, "/api/v1/setup/harness-credential/anthropic"},
	// 2. policies
	{http.MethodPost, "/api/v1/policies"},
	{http.MethodPut, "/api/v1/policies/p1"},
	{http.MethodDelete, "/api/v1/policies/p1"},
	// 3. workspaces
	{http.MethodPost, "/api/v1/workspaces"},
	{http.MethodPut, "/api/v1/workspaces/w1"},
	{http.MethodDelete, "/api/v1/workspaces/w1"},
	{http.MethodPost, "/api/v1/workspaces/w1/scan"},
	{http.MethodPost, "/api/v1/workspaces/w1/build"},
	{http.MethodPut, "/api/v1/workspaces/w1/requirements"},
	{http.MethodPut, "/api/v1/workspaces/w1/approved-egress"},
	{http.MethodPut, "/api/v1/workspaces/w1/llm-cred"},
	{http.MethodPost, "/api/v1/workspaces/w1/record"},
	{http.MethodPost, "/api/v1/workspaces/w1/record/t1/promote-egress"},
	{http.MethodPost, "/api/v1/workspaces/w1/env-as-code/write"},
	// 4. site config
	{http.MethodPut, "/api/v1/site-config"},
	// The two connectivity probes: each LAUNCHES a sandbox, so a member must not
	// be able to fire them (cost + a real outbound request on the operator's behalf).
	{http.MethodPost, "/api/v1/site-config/test-proxy"},
	{http.MethodPost, "/api/v1/site-config/test-redirect"},
	// 5. secrets — credential MATERIAL (the LIST is names-only and stays a read).
	{http.MethodPut, "/api/v1/secrets/s1"},
	{http.MethodDelete, "/api/v1/secrets/s1"},
	// 6. integrations — the row IS a credential binding (secret refs + egress);
	// the LIST/GET stay reads (readRoutes), same split as secrets above.
	{http.MethodPut, "/api/v1/integrations/i1"},
	{http.MethodDelete, "/api/v1/integrations/i1"},
	{http.MethodPost, "/api/v1/integrations/i1/adopt"},
	{http.MethodPost, "/api/v1/integrations/i1/test"},
}

// readRoutes are the reads in those same clusters. A member keeps all of them —
// this gate refuses writes, it does not blind anyone.
//
// NOT here: GET /api/v1/approvals. B2 made it OWNERSHIP-scoped for a member
// (item 2) rather than a flat pass-through, which needs a store/Approvals
// fake that can answer "which runs did this principal create" — rbacServer's
// fakeApprovals models approvals only, not run ownership. Its real (scoped,
// non-500) behavior is covered by the chi.Walk-enumerated matrix in
// authz_test.go instead, same reasoning as the gatedRoutes note above.
var readRoutes = []string{
	"/api/v1/policies",
	"/api/v1/workspaces",
	"/api/v1/site-config",
	"/api/v1/secrets",
}

// TestRequireOperator_ViewerRefusedOnEveryGatedRoute is the finding's regression:
// a signed-in human with the MEMBER role must be refused on every gated route.
func TestRequireOperator_ViewerRefusedOnEveryGatedRoute(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	viewer := ssoSession(t, "sub-viewer", rbacViewer, oidc.RoleMember)
	for _, rt := range gatedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, viewer, "{}")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (member must not reach this handler)", w.Code)
			}
			if !strings.Contains(w.Body.String(), "requires admin role") {
				t.Errorf("body = %q, want the admin-role message", w.Body.String())
			}
			if strings.Contains(w.Body.String(), rbacOperator) {
				t.Errorf("403 body leaks the operator allowlist: %q", w.Body.String())
			}
		})
	}
}

// TestRequireOperator_OperatorPassesEveryGatedRoute pins the other direction: an
// admin-role session is never stopped by the gate (the handler then answers on
// its own merits — a 4xx for the deliberately invalid body/id here, never a
// 403/401).
func TestRequireOperator_OperatorPassesEveryGatedRoute(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	op := ssoSession(t, "sub-op", "ops@corp.example", oidc.RoleAdmin)
	for _, rt := range gatedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, op, "{}")
			if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
				t.Fatalf("status = %d, want the handler to run for an admin: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestRequireOperator_ViewerKeepsReads: read routes are untouched by the gate.
func TestRequireOperator_ViewerKeepsReads(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	viewer := ssoSession(t, "sub-viewer", rbacViewer, oidc.RoleMember)
	for _, path := range readRoutes {
		t.Run(path, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, path, viewer, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 for a member: %s", path, w.Code, w.Body.String())
			}
		})
	}
}

// TestRequireOperator_RoleGatesNotEmailList pins the B2 unification (item 1):
// isOperator now reads the session's ROLE, never the OperatorEmails allowlist
// directly. An email ON the allowlist with role=member is still refused, and an
// email NOT on the allowlist with role=admin still passes — the allowlist's
// only remaining effect is upstream, feeding oidc.Config.LegacyAdminEmails at
// login (internal/auth/oidc's deriveRole, tested there); internal/api no longer
// consults Config.OperatorEmails at all.
func TestRequireOperator_RoleGatesNotEmailList(t *testing.T) {
	srv := rbacServer(t, rbacOperator) // rbacOperator IS on the allowlist
	memberOnList := ssoSession(t, "sub-1", rbacOperator, oidc.RoleMember)
	adminOffList := ssoSession(t, "sub-2", rbacViewer, oidc.RoleAdmin)
	for _, rt := range gatedRoutes {
		t.Run("listed-email member-role "+rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, memberOnList, "{}")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (role, not the allowlist, gates)", w.Code)
			}
		})
		t.Run("unlisted-email admin-role "+rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, adminOffList, "{}")
			if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
				t.Fatalf("status = %d, want the handler to run (role, not the allowlist, gates): %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestRequireOperator_AdminTokenAlwaysOperator: the shared admin token has no
// human identity to demote, so it stays an operator even with a list configured
// (the CLI must keep working).
func TestRequireOperator_AdminTokenAlwaysOperator(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	for _, rt := range gatedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := do(t, srv, rt.method, rt.path, adminToken, "{}")
			if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
				t.Fatalf("status = %d, want the admin token to stay an operator: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestMeReportsOperatorRole: the console cannot hide the admin-only actions from
// a member unless the API tells it the role — otherwise the tier is discoverable
// only as a raw 403 after the click. handleMe answers with the SAME isOperator
// predicate requireOperator gates the routes with, so the badge and the gate can
// never disagree. Also pins the two fields item 7 added: role and email.
func TestMeReportsOperatorRole(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	for _, tc := range []struct {
		name, sub, email, sessionRole string
		wantOperator                  bool
	}{
		{"admin role is operator", "sub-op", "ops@corp.example", oidc.RoleAdmin, true},
		{"member role is not operator", "sub-viewer", rbacViewer, oidc.RoleMember, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/me", ssoSession(t, tc.sub, tc.email, tc.sessionRole), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /me = %d, want 200: %s", w.Code, w.Body.String())
			}
			var got struct {
				Principal string `json:"principal"`
				Method    string `json:"method"`
				Role      string `json:"role"`
				Email     string `json:"email"`
				Operator  bool   `json:"operator"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode /me: %v (body %q)", err, w.Body.String())
			}
			if got.Operator != tc.wantOperator {
				t.Fatalf("/me operator = %v, want %v (body %q)", got.Operator, tc.wantOperator, w.Body.String())
			}
			if got.Role != tc.sessionRole {
				t.Fatalf("/me role = %q, want %q (body %q)", got.Role, tc.sessionRole, w.Body.String())
			}
			if got.Email != tc.email {
				t.Fatalf("/me email = %q, want %q (body %q)", got.Email, tc.email, w.Body.String())
			}
			// operator must always agree with role==admin — the console reads
			// both, and they must never be able to disagree.
			if (got.Role == oidc.RoleAdmin) != got.Operator {
				t.Fatalf("/me role/operator disagree: role=%q operator=%v", got.Role, got.Operator)
			}
			// The two pre-existing fields must survive — the console reads them.
			if got.Principal == "" || got.Method == "" {
				t.Fatalf("/me dropped principal/method: %q", w.Body.String())
			}
		})
	}
}
