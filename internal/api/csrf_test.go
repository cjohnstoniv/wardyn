// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ─── CSRF: the same-origin guard on a COOKIE-authenticated mutating request ──
//
// Two guards, one rule, pinned together. LocalMode has refused a cross-origin
// mutating request since FIX #8's sibling (http.go's local arm) but nothing
// ever tested it; the OIDC session branch had no check at all through 0.7.2
// and leaned on the cookie's SameSite=Lax alone. This table drives the REAL
// middleware end to end (router included) in BOTH modes, so neither half can
// drift without a red test.
//
// The bearer lane is exempt BY CONSTRUCTION (it never enters the session
// branch) and is pinned separately below — a token is not ambient authority a
// browser attaches for the attacker.

const (
	// csrfLocalHost is the Host a local-mode request must carry (the loopback
	// gate runs first, so every local row uses it).
	csrfLocalHost = "127.0.0.1:8080"
	// csrfOIDCHost is the Host wardynd sees behind a TLS-terminating ingress —
	// deliberately NOT the browser-facing name in csrfRedirectURL.
	csrfOIDCHost     = "wardyn.internal:8080"
	csrfRedirectURL  = "https://console.wardyn.example/auth/callback"
	csrfRedirectHost = "https://console.wardyn.example"

	// originSelf is a table sentinel: the mode's own "http://" + r.Host.
	originSelf = "<self>"
	// originSelfUpper is originSelf with the host upper-cased (host comparison
	// is case-insensitive — a browser may send either).
	originSelfUpper = "<SELF>"
)

// csrfLocalServer is the LocalMode fixture — the same shape
// TestLocalModeRejectsNonLoopbackHost uses, so the loopback gates that run
// BEFORE the CSRF guard behave identically here.
func csrfLocalServer(t *testing.T) *Server {
	t.Helper()
	return New(Config{
		Identity:      mustIDP(t),
		Approvals:     newFakeApprovals(),
		Broker:        &fakeBroker{},
		Audit:         &recRecorder{},
		LocalMode:     true,
		LocalOperator: "local:tester",
		DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
	})
}

// csrfOIDCServer is the SSO fixture: a zero-value Authenticator (the key
// ssoSession signs for), plus the OIDCRedirectURL the guard treats as a second
// same-origin host.
func csrfOIDCServer(t *testing.T) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.OIDCRedirectURL = csrfRedirectURL
	return New(cfg)
}

// csrfDo issues one request against srv with the given headers. cookie is nil
// for the local-mode rows (there is no session to mint) and the SSO session for
// the OIDC rows.
func csrfDo(t *testing.T, srv *Server, method, path, host, origin, secFetchSite string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, path, nil)
	r.Host = host
	r.RemoteAddr = "127.0.0.1:54321" // loopback peer: isolates the CSRF guard from N1's peer gate
	if origin != "" {
		r.Header.Set("Origin", origin)
	}
	if secFetchSite != "" {
		r.Header.Set("Sec-Fetch-Site", secFetchSite)
	}
	if cookie != nil {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	return w
}

// TestCSRFGuard is the table: {local, oidc} × origin/fetch-metadata shapes ×
// {GET, POST}. GET is never refused (the guard is mutating-methods only); POST
// is refused exactly where wantLocalRefused/wantOIDCRefused say so.
func TestCSRFGuard(t *testing.T) {
	cases := []struct {
		name             string
		origin           string // "" = header absent; originSelf/originSelfUpper = this mode's own host
		secFetchSite     string
		wantLocalRefused bool
		wantOIDCRefused  bool
	}{
		{
			name: "no Origin, no Sec-Fetch-Site (CLI/API client)",
		},
		{
			name:   "same-origin Origin (r.Host)",
			origin: originSelf,
		},
		{
			name:   "same-origin Origin, host case-folded",
			origin: originSelfUpper,
		},
		{
			name:         "same-origin Origin plus Sec-Fetch-Site: same-origin",
			origin:       originSelf,
			secFetchSite: "same-origin",
		},
		{
			name:         "no Origin, Sec-Fetch-Site: none (user-initiated)",
			secFetchSite: "none",
		},
		{
			// The TLS-terminating-ingress row: the browser's Origin is the
			// public console name, which r.Host is not. LocalMode must still
			// refuse it — a loopback deployment has no ingress and the
			// redirect URL is not consulted there.
			name:             "Origin is the OIDCRedirectURL host (ingress)",
			origin:           csrfRedirectHost,
			wantLocalRefused: true,
		},
		{
			name:             "cross-origin Origin",
			origin:           "https://evil.example",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			// Suffix/prefix confusion: a host that merely CONTAINS ours.
			name:             "cross-origin Origin whose host extends ours",
			origin:           "https://wardyn.internal.evil.example",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			name:             "Sec-Fetch-Site: cross-site with no Origin",
			secFetchSite:     "cross-site",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			// A forged Sec-Fetch-Site must not whitewash a hostile Origin:
			// both checks run, either one refuses.
			name:             "cross-origin Origin with a forged Sec-Fetch-Site: same-origin",
			origin:           "https://evil.example",
			secFetchSite:     "same-origin",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			name:             "Origin: null (opaque / sandboxed document)",
			origin:           "null",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			name:             "malformed Origin (unparseable)",
			origin:           "http://[",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			name:             "Origin with no host at all",
			origin:           "file:///etc/passwd",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
	}

	for _, mode := range []struct {
		name     string
		host     string
		local    bool
		passGET  int
		passPOST int
	}{
		// LocalMode: no auth at all, POST /auth/logout answers 204 (OIDC nil).
		{name: "local", host: csrfLocalHost, local: true, passGET: http.StatusOK, passPOST: http.StatusNoContent},
		// OIDC: the session cookie authenticates; LogoutHandler redirects (302).
		{name: "oidc", host: csrfOIDCHost, passGET: http.StatusOK, passPOST: http.StatusFound},
	} {
		t.Run(mode.name, func(t *testing.T) {
			for _, c := range cases {
				c := c
				t.Run(c.name, func(t *testing.T) {
					var srv *Server
					var cookie *http.Cookie
					if mode.local {
						srv = csrfLocalServer(t)
					} else {
						srv = csrfOIDCServer(t)
						cookie = ssoSession(t, "sso-human", "human@corp.example", oidc.RoleAdmin)
					}
					origin := c.origin
					switch origin {
					case originSelf:
						origin = "http://" + mode.host
					case originSelfUpper:
						origin = "http://" + strings.ToUpper(mode.host)
					}
					wantRefused := c.wantOIDCRefused
					if mode.local {
						wantRefused = c.wantLocalRefused
					}

					// GET is never a state change: every row must pass.
					w := csrfDo(t, srv, http.MethodGet, "/api/v1/me", mode.host, origin, c.secFetchSite, cookie)
					if w.Code != mode.passGET {
						t.Errorf("GET /api/v1/me: code = %d, want %d (the guard must not touch a read)\nbody: %s", w.Code, mode.passGET, w.Body.String())
					}

					w = csrfDo(t, srv, http.MethodPost, "/api/v1/auth/logout", mode.host, origin, c.secFetchSite, cookie)
					if wantRefused {
						if w.Code != http.StatusForbidden {
							t.Fatalf("POST /api/v1/auth/logout: code = %d, want 403 (cross-origin mutation must be refused)\nbody: %s", w.Code, w.Body.String())
						}
						if !strings.Contains(w.Body.String(), csrfRefusedBody) {
							t.Errorf("POST refusal body = %q, want it to contain %q", w.Body.String(), csrfRefusedBody)
						}
						return
					}
					if w.Code != mode.passPOST {
						t.Fatalf("POST /api/v1/auth/logout: code = %d, want %d (a same-origin mutation must pass)\nbody: %s", w.Code, mode.passPOST, w.Body.String())
					}
				})
			}
		})
	}
}

// TestCSRFGuard_BearerCallerIsNotRefused is the exemption control. The guard
// hangs off the OIDC SESSION branch only: a bearer token is not ambient
// authority a browser attaches to a cross-site request, so a CLI/CI caller
// carrying an attacker's Origin is NOT the CSRF threat and must keep working.
// If someone ever moves the guard up in front of the whole middleware, this
// goes red.
func TestCSRFGuard_BearerCallerIsNotRefused(t *testing.T) {
	srv := csrfOIDCServer(t)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r.Host = csrfOIDCHost
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Authorization", "Bearer "+adminToken)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	srv.Handler().ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("bearer caller with a cross-site Origin: code = %d, want 302 (exempt by construction)\nbody: %s", w.Code, w.Body.String())
	}
}
