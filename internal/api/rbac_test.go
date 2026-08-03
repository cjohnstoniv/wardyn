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
func ssoSession(t *testing.T, sub, email string) *http.Cookie {
	t.Helper()
	payload, err := json.Marshal(oidc.Session{Sub: sub, Email: email, Expiry: time.Now().UTC().Add(time.Hour)})
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

// operatorCtx models what humanOrAdminAuth publishes for a verified SSO human.
func operatorCtx(sub, email string) context.Context {
	return withOIDCEmail(withOIDCHuman(context.Background(), sub), email)
}

// TestIsOperator covers the role resolution itself: who is an operator, and the
// two ways a caller can be one without appearing on the list (admin token,
// local mode — a single shared credential carries no human identity).
func TestIsOperator(t *testing.T) {
	tests := []struct {
		name  string
		list  []string
		ctx   context.Context
		wantP bool
	}{
		{"no list: sso human is operator", nil, operatorCtx("sub-1", rbacViewer), true},
		{"no list: admin token is operator", nil, context.Background(), true},
		{"listed email is operator", []string{rbacOperator}, operatorCtx("sub-1", rbacOperator), true},
		{"match is case-insensitive", []string{rbacOperator}, operatorCtx("sub-1", "ops@CORP.example"), true},
		{"list entries are trimmed", []string{"  " + rbacOperator + " "}, operatorCtx("sub-1", rbacOperator), true},
		{"unlisted email is viewer", []string{rbacOperator}, operatorCtx("sub-2", rbacViewer), false},
		{"sso human with no email is viewer", []string{rbacOperator}, operatorCtx("sub-3", ""), false},
		// EqualFold does Unicode SIMPLE folding: U+017F (ſ) folds onto ASCII "s",
		// so without the ASCII guard this crafted IdP address would MATCH an
		// ASCII allowlist entry — the escalating direction. Fail closed instead.
		{"non-ascii email never matches (fold-escalation)", []string{"ross@corp.example"}, operatorCtx("sub-4", "roſs@corp.example"), false},
		{"admin token is operator even with a list", []string{rbacOperator}, context.Background(), true},
		{"local mode is operator even with a list", []string{rbacOperator}, withLocalPrincipal(context.Background(), "local:alice"), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := &Server{cfg: Config{OperatorEmails: tc.list}}
			if got := s.isOperator(tc.ctx); got != tc.wantP {
				t.Fatalf("isOperator = %v, want %v", got, tc.wantP)
			}
		})
	}
}

// gatedRoutes is the FULL set requireOperator gates, i.e. the operator half of
// the tier split (viewer = read + launch runs). It is the test's copy of the
// intent, so adding a mutating route to one of these clusters without gating it
// fails here.
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
	{http.MethodPut, "/api/v1/workspaces/w1/approved-egress"},
	{http.MethodPut, "/api/v1/workspaces/w1/llm-cred"},
	{http.MethodPut, "/api/v1/workspaces/w1/setup-commands"},
	{http.MethodPost, "/api/v1/workspaces/w1/verify"},
	{http.MethodPost, "/api/v1/workspaces/w1/record"},
	{http.MethodPost, "/api/v1/workspaces/w1/record/t1/promote-egress"},
	{http.MethodPost, "/api/v1/workspaces/w1/finalize"},
	{http.MethodPost, "/api/v1/workspaces/w1/verify/suggest-fix"},
	// 4. site config
	{http.MethodPut, "/api/v1/site-config"},
	// 5. secrets — credential MATERIAL (the LIST is names-only and stays a read).
	{http.MethodPut, "/api/v1/secrets/s1"},
	{http.MethodDelete, "/api/v1/secrets/s1"},
	// 6. approval decisions — the live authorization over an escalation. Reading
	// the queue stays a viewer act (see readRoutes).
	{http.MethodPost, "/api/v1/approvals/a1/approve"},
	{http.MethodPost, "/api/v1/approvals/a1/deny"},
	// 7. attach — both lanes to a live interactive PTY. Gating only the ticket
	// mint would buy nothing: the WS route falls through to cookie auth when no
	// ?ticket= is presented, and a browser attaches a same-origin session cookie
	// to a WebSocket handshake automatically, so a viewer would simply omit the
	// ticket and get the same PTY. Both are listed because both must refuse.
	{http.MethodPost, "/api/v1/runs/r1/attach-ticket"},
	{http.MethodGet, "/api/v1/runs/r1/attach"},
}

// readRoutes are the reads in those same clusters. A viewer keeps all of them —
// this gate refuses writes, it does not blind anyone.
var readRoutes = []string{
	"/api/v1/policies",
	"/api/v1/workspaces",
	"/api/v1/site-config",
	"/api/v1/secrets",
	"/api/v1/approvals",
}

// TestRequireOperator_ViewerRefusedOnEveryGatedRoute is the finding's regression:
// with an operator allowlist configured, a signed-in human who is not on it must
// be refused on every gated route.
func TestRequireOperator_ViewerRefusedOnEveryGatedRoute(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	viewer := ssoSession(t, "sub-viewer", rbacViewer)
	for _, rt := range gatedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, viewer, "{}")
			if w.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403 (viewer must not reach this handler)", w.Code)
			}
			if !strings.Contains(w.Body.String(), "requires operator role") {
				t.Errorf("body = %q, want the operator-role message", w.Body.String())
			}
			if strings.Contains(w.Body.String(), rbacOperator) {
				t.Errorf("403 body leaks the operator allowlist: %q", w.Body.String())
			}
		})
	}
}

// TestRequireOperator_OperatorPassesEveryGatedRoute pins the other direction: a
// listed human is never stopped by the gate (the handler then answers on its own
// merits — a 4xx for the deliberately invalid body/id here, never a 403/401).
func TestRequireOperator_OperatorPassesEveryGatedRoute(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	op := ssoSession(t, "sub-op", "ops@corp.example") // lower-case: list is mixed
	for _, rt := range gatedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, op, "{}")
			if w.Code == http.StatusForbidden || w.Code == http.StatusUnauthorized {
				t.Fatalf("status = %d, want the handler to run for an operator: %s", w.Code, w.Body.String())
			}
		})
	}
}

// TestRequireOperator_ViewerKeepsReads: read routes are untouched by the gate.
func TestRequireOperator_ViewerKeepsReads(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	viewer := ssoSession(t, "sub-viewer", rbacViewer)
	for _, path := range readRoutes {
		t.Run(path, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, path, viewer, "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET %s = %d, want 200 for a viewer: %s", path, w.Code, w.Body.String())
			}
		})
	}
}

// TestRequireOperator_UnsetListChangesNothing is the compatibility guarantee: no
// allowlist configured => no viewers => the same human who is refused above sails
// through every gated route, exactly as before this gate existed.
func TestRequireOperator_UnsetListChangesNothing(t *testing.T) {
	srv := rbacServer(t) // no operator emails
	sess := ssoSession(t, "sub-anyone", rbacViewer)
	for _, rt := range gatedRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			w := doSSO(t, srv, rt.method, rt.path, sess, "{}")
			if w.Code == http.StatusForbidden {
				t.Fatalf("403 with no operator allowlist configured — the gate is not additive: %s", w.Body.String())
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

// TestMeReportsOperatorRole: the console cannot hide the operator-only actions
// from a viewer unless the API tells it the role — otherwise the tier is
// discoverable only as a raw 403 after the click. handleMe answers with the SAME
// isOperator predicate requireOperator gates the routes with, so the badge and
// the gate can never disagree.
func TestMeReportsOperatorRole(t *testing.T) {
	srv := rbacServer(t, rbacOperator)
	for _, tc := range []struct {
		name, sub, email string
		want             bool
	}{
		{"listed human is operator", "sub-op", "ops@corp.example", true}, // lower-case: list is mixed
		{"unlisted human is viewer", "sub-viewer", rbacViewer, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := doSSO(t, srv, http.MethodGet, "/api/v1/me", ssoSession(t, tc.sub, tc.email), "")
			if w.Code != http.StatusOK {
				t.Fatalf("GET /me = %d, want 200: %s", w.Code, w.Body.String())
			}
			var got struct {
				Principal string `json:"principal"`
				Method    string `json:"method"`
				Operator  bool   `json:"operator"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode /me: %v (body %q)", err, w.Body.String())
			}
			if got.Operator != tc.want {
				t.Fatalf("/me operator = %v, want %v (body %q)", got.Operator, tc.want, w.Body.String())
			}
			// The two pre-existing fields must survive — the console reads them.
			if got.Principal == "" || got.Method == "" {
				t.Fatalf("/me dropped principal/method: %q", w.Body.String())
			}
		})
	}
}
