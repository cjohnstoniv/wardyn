// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
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
// same-origin host. redirectURL is a parameter because how an operator SPELLS
// that URL (with or without the scheme's default port, or not at all) is itself
// under test — see the ingress rows.
func csrfOIDCServer(t *testing.T, redirectURL string) *Server {
	t.Helper()
	h := newHarness(t)
	cfg := baseTestConfig(h, rbacStore{})
	cfg.OIDC = &oidc.Authenticator{}
	cfg.OIDCRedirectURL = redirectURL
	return New(cfg)
}

// csrfDo issues one request against srv with the given headers. cookie is nil
// for the local-mode rows (there is no session to mint) and the SSO session for
// the OIDC rows.
func csrfDo(t *testing.T, srv *Server, method, path, host, origin, secFetchSite, body string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == "" {
		r = httptest.NewRequest(method, path, nil)
	} else {
		r = httptest.NewRequest(method, path, strings.NewReader(body))
	}
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
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	return w
}

// TestCSRFGuard is the table: {local, oidc} × origin/fetch-metadata shapes ×
// {GET, POST}. GET is never refused (the guard is mutating-methods only); POST
// is refused exactly where wantLocalRefused/wantOIDCRefused say so.
func TestCSRFGuard(t *testing.T) {
	cases := []struct {
		name         string
		origin       string // "" = header absent; originSelf/originSelfUpper = this mode's own host
		secFetchSite string
		// redirectURL overrides the OIDC fixture's WARDYN_OIDC_REDIRECT_URL
		// ("" = csrfRedirectURL). LocalMode never reads it.
		redirectURL      string
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
			// DEFAULT-PORT NORMALIZATION, the operator's side: the redirect URL
			// spells :443, the browser never does. Without the drop this arm
			// evaporates and — behind an ingress, where r.Host is the internal
			// name — EVERY console write 403s on a config that looks right.
			name:        "Origin omits the default :443 the redirect URL spells",
			origin:      csrfRedirectHost,
			redirectURL: "https://console.wardyn.example:443/auth/callback",
			// LocalMode does not consult the redirect URL at all: a
			// non-loopback Origin is refused there whatever it says.
			wantLocalRefused: true,
		},
		{
			// The same rule from the browser's side (a page served on an
			// explicit :443), against a redirect URL written without it.
			name:             "Origin spells the default :443 the redirect URL omits",
			origin:           csrfRedirectHost + ":443",
			wantLocalRefused: true,
		},
		{
			// THE THREAT-MODEL ROW'S HEADLINE CLAIM: SameSite=Lax does not bind
			// a sibling host on a shared parent domain. That attacker's browser
			// labels the request "same-site", NOT "cross-site" — so the
			// deliberate != "cross-site" fallthrough is what has to refuse it,
			// on the Origin's own merits.
			name:             "Sec-Fetch-Site: same-site with a sibling-host Origin",
			origin:           "https://sibling.wardyn.example",
			secFetchSite:     "same-site",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			// ACCEPTED BEHAVIOUR CHANGE (0.7.3), pinned so it is a decision and
			// not a surprise: browsers treat localhost and 127.0.0.1 as two
			// different SITES, so a page at http://localhost:<port> posting to
			// http://127.0.0.1:<port> now carries Sec-Fetch-Site: cross-site
			// and is refused — 0.7.2 allowed it, because the loopback-Origin
			// rule alone could not see the difference. Nothing Wardyn serves
			// does this: the console fetches relative URLs, same-origin.
			name:             "LocalMode loopback alias: a localhost page posting to 127.0.0.1",
			origin:           "http://localhost:8080",
			secFetchSite:     "cross-site",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
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
			// RULE 3's OWN WORDS (S2-02): the CLI/API fallthrough is "no Origin
			// AND no Sec-Fetch-Site". A browser that omits Origin on a same-site
			// top-level form POST — the sibling host on a shared parent domain,
			// again — used to land in it, because only "cross-site" refused.
			name:             "Sec-Fetch-Site: same-site with no Origin",
			secFetchSite:     "same-site",
			wantLocalRefused: true,
			wantOIDCRefused:  true,
		},
		{
			// The same tightening from the other side: a label the BROWSER set
			// to something other than same-origin/none is refused whatever the
			// Origin says. Nothing the console does reaches this — its own
			// fetches are same-origin — and no CLI sets the header at all.
			name:             "Sec-Fetch-Site: same-site with our own Origin",
			origin:           originSelf,
			secFetchSite:     "same-site",
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
				t.Run(c.name, func(t *testing.T) {
					var srv *Server
					var cookie *http.Cookie
					if mode.local {
						srv = csrfLocalServer(t)
					} else {
						redirect := c.redirectURL
						if redirect == "" {
							redirect = csrfRedirectURL
						}
						srv = csrfOIDCServer(t, redirect)
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
					w := csrfDo(t, srv, http.MethodGet, "/api/v1/me", mode.host, origin, c.secFetchSite, "", cookie)
					if w.Code != mode.passGET {
						t.Errorf("GET /api/v1/me: code = %d, want %d (the guard must not touch a read)\nbody: %s", w.Code, mode.passGET, w.Body.String())
					}

					w = csrfDo(t, srv, http.MethodPost, "/api/v1/auth/logout", mode.host, origin, c.secFetchSite, "", cookie)
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
	srv := csrfOIDCServer(t, csrfRedirectURL)
	r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	r.Host = csrfOIDCHost
	r.RemoteAddr = "127.0.0.1:54321"
	r.Header.Set("Authorization", "Bearer "+adminToken)
	r.Header.Set("Origin", "https://evil.example")
	r.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("bearer caller with a cross-site Origin: code = %d, want 302 (exempt by construction)\nbody: %s", w.Code, w.Body.String())
	}
}

// TestCSRFGuard_EveryMutatingRouteIsFenced is the REGRESSION FENCE, and it
// hand-lists nothing. The table above drives one route (POST /auth/logout, the
// most harmless mutation in the API); this walks authz_test.go's
// chi.Walk-derived routeMatrix — the file's own doctrine, and the thing that
// notices a route the router gained — and asserts that EVERY registered
// mutating route reachable by a session cookie refuses a cross-site request.
//
// The realistic way this protection is lost is not someone deleting the guard:
// it is a new route registered OUTSIDE the r.Use(s.humanOrAdminAuth) group, or
// a group reshuffle that moves one out. Either goes red here.
func TestCSRFGuard_EveryMutatingRouteIsFenced(t *testing.T) {
	// The MAXIMALLY-CONFIGURED server the matrix itself is built from
	// (Secrets, RecordingStore, SessionRevocations all wired) — anything less
	// 404s the conditionally-mounted routes and the fence would read those as
	// "not reachable" instead of "not fenced".
	srv, _, _, _ := newAuthzMatrixServer(t)
	cookie := ssoSession(t, "sso-human", "human@corp.example", oidc.RoleAdmin)

	fenced := 0
	for key, rc := range routeMatrix {
		method, pattern, ok := strings.Cut(key, " ")
		if !ok {
			t.Fatalf("malformed routeMatrix key %q", key)
		}
		if !isMutatingMethod(method) {
			continue
		}
		switch rc.class {
		case classAnonymous:
			// No credential is consulted, so there is no ambient authority to
			// forge — the CSRF question does not arise.
			continue
		case classInternal, classDevice:
			// Run-token / ground-truth / device BEARER credentials: no cookie,
			// exempt by construction exactly as the admin bearer is.
			continue
		}
		fenced++
		t.Run(key, func(t *testing.T) {
			p := buildPath(pattern, "x1")
			w := csrfDo(t, srv, method, p, csrfOIDCHost, "https://evil.example", "cross-site", bodyFor(method, rc), cookie)
			if w.Code != http.StatusForbidden {
				t.Fatalf("cross-site %s: code = %d, want 403 — this route is session-reachable and NOT behind the CSRF guard\nbody: %s", key, w.Code, w.Body.String())
			}
			// A 403 alone is not proof: classAdmin routes 403 a member anyway.
			// The body is what says WHICH gate refused.
			if !strings.Contains(w.Body.String(), csrfRefusedBody) {
				t.Fatalf("cross-site %s: 403 came from a different gate (body %q), not the CSRF guard", key, w.Body.String())
			}
		})
	}
	// A matrix that stopped yielding routes would make this test vacuously
	// green — the exact failure the walk exists to prevent.
	if fenced < 50 {
		t.Fatalf("fenced only %d mutating session-reachable routes; the routeMatrix walk yields ~70 — something stopped enumerating", fenced)
	}
}

// TestCSRFGuard_RefusalIsAudited pins the SIEM half in both modes: a refusal
// emits the EXISTING auth.failed action with reason cross_origin_refused, so
// "is someone attacking this" and "why did the console stop saving" are
// answerable from the trail. The guard short-circuits above adminAuth (the
// chokepoint every other public-API refusal funnels through), so without an
// explicit emit a threat-model-registered control would leave no trace.
func TestCSRFGuard_RefusalIsAudited(t *testing.T) {
	t.Run("oidc", func(t *testing.T) {
		h := newHarness(t)
		cfg := baseTestConfig(h, rbacStore{})
		cfg.OIDC = &oidc.Authenticator{}
		cfg.OIDCRedirectURL = csrfRedirectURL
		srv := New(cfg)
		cookie := ssoSession(t, "sso-human", "human@corp.example", oidc.RoleAdmin)
		w := csrfDo(t, srv, http.MethodPost, "/api/v1/auth/logout", csrfOIDCHost, "https://evil.example", "", "", cookie)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		assertCSRFAudited(t, h.audit.events, "/api/v1/auth/logout")
	})

	t.Run("local", func(t *testing.T) {
		audit := &recRecorder{}
		srv := New(Config{
			Identity:      mustIDP(t),
			Approvals:     newFakeApprovals(),
			Broker:        &fakeBroker{},
			Audit:         audit,
			LocalMode:     true,
			LocalOperator: "local:tester",
			DefaultPolicy: types.RunPolicySpec{MinConfinementClass: types.CC2},
		})
		w := csrfDo(t, srv, http.MethodPost, "/api/v1/auth/logout", csrfLocalHost, "https://evil.example", "", "", nil)
		if w.Code != http.StatusForbidden {
			t.Fatalf("code = %d, want 403", w.Code)
		}
		assertCSRFAudited(t, audit.events, "/api/v1/auth/logout")
	})
}

// assertCSRFAudited finds the auth.failed row and checks the shape every other
// refusal in this middleware writes: system actor, failure outcome, the request
// path as Target, a source IP, and the content-free reason.
func assertCSRFAudited(t *testing.T, events []types.AuditEvent, path string) {
	t.Helper()
	var ev *types.AuditEvent
	for i := range events {
		if events[i].Action == "auth.failed" {
			ev = &events[i]
		}
	}
	if ev == nil {
		t.Fatalf("no auth.failed audit event recorded for a CSRF refusal; events = %+v", events)
	}
	if ev.ActorType != types.ActorSystem || ev.Outcome != "failure" {
		t.Errorf("ActorType/Outcome = %q/%q, want system/failure", ev.ActorType, ev.Outcome)
	}
	if ev.Target != path {
		t.Errorf("Target = %q, want the request path %q", ev.Target, path)
	}
	if ev.SourceIP == "" {
		t.Error("SourceIP not set")
	}
	var data map[string]any
	if err := json.Unmarshal(ev.Data, &data); err != nil {
		t.Fatalf("Data unmarshal: %v", err)
	}
	if data["reason"] != csrfAuditReason {
		t.Errorf("reason = %v, want %q", data["reason"], csrfAuditReason)
	}
}

// TestCSRFGuard_NoRedirectURLMeansOnlyRHost: with SSO configured but no
// redirect URL parseable to a host, the second accepted name simply does not
// exist — r.Host is the only same-origin name, and everything else is refused.
// Boot refuses that configuration outright since 0.7.3
// (cmd/wardynd.validateOIDCRedirectURL); this pins that the API layer does not
// fail OPEN if one ever reaches it anyway.
func TestCSRFGuard_NoRedirectURLMeansOnlyRHost(t *testing.T) {
	for _, redirect := range []string{"", "console.wardyn.example/auth/callback"} {
		srv := csrfOIDCServer(t, redirect)
		cookie := ssoSession(t, "sso-human", "human@corp.example", oidc.RoleAdmin)
		w := csrfDo(t, srv, http.MethodPost, "/api/v1/auth/logout", csrfOIDCHost, csrfRedirectHost, "", "", cookie)
		if w.Code != http.StatusForbidden {
			t.Errorf("redirect URL %q: code = %d, want 403 (no host to accept)", redirect, w.Code)
		}
		w = csrfDo(t, srv, http.MethodPost, "/api/v1/auth/logout", csrfOIDCHost, "http://"+csrfOIDCHost, "", "", cookie)
		if w.Code != http.StatusFound {
			t.Errorf("redirect URL %q: same-origin (r.Host) code = %d, want 302 — r.Host must still be accepted", redirect, w.Code)
		}
	}
}

// TestOriginHost is the predicate table: what counts as a comparable host, and
// what fails closed. Kept separate from the HTTP table because these shapes are
// cheaper and clearer to state directly than to drive a server for.
func TestOriginHost(t *testing.T) {
	for _, c := range []struct {
		in       string
		wantHost string
		wantOK   bool
	}{
		{in: "https://console.example", wantHost: "console.example", wantOK: true},
		// The default-port drop, both directions and both schemes.
		{in: "https://console.example:443", wantHost: "console.example", wantOK: true},
		{in: "http://console.example:80", wantHost: "console.example", wantOK: true},
		// A NON-default port is part of the origin and stays.
		{in: "https://console.example:8443", wantHost: "console.example:8443", wantOK: true},
		{in: "http://console.example:8080", wantHost: "console.example:8080", wantOK: true},
		// :443 on http (and :80 on https) is NOT that scheme's default: keep it.
		{in: "http://console.example:443", wantHost: "console.example:443", wantOK: true},
		// A full URL with a path is accepted — WARDYN_OIDC_REDIRECT_URL is one.
		{in: "https://console.example/auth/callback", wantHost: "console.example", wantOK: true},
		// IPv6 keeps its brackets, which is what r.Host carries.
		{in: "http://[::1]:8080", wantHost: "[::1]:8080", wantOK: true},
		{in: "https://[::1]:443", wantHost: "[::1]", wantOK: true},
		// Fail closed.
		{in: "null"},
		{in: ""},
		{in: "http://["},
		{in: "file:///etc/passwd"},
		{in: "console.example/auth/callback"}, // no scheme: not a serialised origin
		{in: "//console.example"},             // scheme-relative reference
		{in: "https://user@console.example"},  // userinfo: the host is not where a reader looks
	} {
		t.Run(c.in, func(t *testing.T) {
			host, ok := originHost(c.in)
			if ok != c.wantOK || host != c.wantHost {
				t.Fatalf("originHost(%q) = (%q, %v), want (%q, %v)", c.in, host, ok, c.wantHost, c.wantOK)
			}
		})
	}
}

// TestAttachOriginRefused pins the PTY-attach socket's origin decision: r.Host
// always, the CSRF guard's second host when SSO is configured, an absent Origin
// (a non-browser client, which is what the library itself allows), and nothing
// else — including when the redirect URL has no readable host at all.
func TestAttachOriginRefused(t *testing.T) {
	attach := func(srv *Server, origin string) bool {
		r := httptest.NewRequest(http.MethodGet, "/api/v1/runs/x/attach", nil)
		r.Host = csrfOIDCHost
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return srv.attachOriginRefused(r)
	}
	srv := csrfOIDCServer(t, csrfRedirectURL)
	for _, allowed := range []string{"", "http://" + csrfOIDCHost, csrfRedirectHost, "https://CONSOLE.wardyn.example:443"} {
		if attach(srv, allowed) {
			t.Errorf("Origin %q refused; want allowed", allowed)
		}
	}
	for _, refused := range []string{"https://evil.example", "null", "http://[", "file:///x", "https://u@console.wardyn.example"} {
		if !attach(srv, refused) {
			t.Errorf("Origin %q allowed; want refused", refused)
		}
	}
	// No readable redirect host ⇒ same-origin only, never fail-open.
	for _, redirect := range []string{"", "console.wardyn.example/auth/callback"} {
		bare := csrfOIDCServer(t, redirect)
		if !attach(bare, csrfRedirectHost) {
			t.Errorf("redirect URL %q: the second host was accepted anyway", redirect)
		}
		if attach(bare, "http://"+csrfOIDCHost) {
			t.Errorf("redirect URL %q: r.Host itself was refused", redirect)
		}
	}
}
