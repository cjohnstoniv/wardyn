// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// #1241: the enter ticket is bound to the browser that asked for it, and a
// relayed app cannot register a service worker outside its own prefix.

const uiBindUnboundReason = "ticket not bound to this browser"

// enterUnbound drives enter the way a browser that never bound the ticket
// does: a victim's browser pushed through the hand-off by someone else's page.
func (h *uiHarness) enterUnbound(method string, v url.Values, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	h.t.Helper()
	var req *http.Request
	if method == http.MethodGet {
		req = httptest.NewRequest(http.MethodGet, uiEnterPath+"?"+v.Encode(), nil)
	} else {
		req = httptest.NewRequest(http.MethodPost, uiEnterPath, strings.NewReader(v.Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		req.AddCookie(c)
	}
	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, req)
	return rec
}

// TestUIGateway_EnterRefusesATicketThisBrowserDidNotBind is the login-CSRF
// proof: a ticket minted and bound in browser A is refused in browser B, by
// GET and by POST, with the bad-ticket 403 and an audit row — and B's attempt
// does not spend it, so A still gets in.
func TestUIGateway_EnterRefusesATicketThisBrowserDidNotBind(t *testing.T) {
	for _, tc := range []struct {
		method string
		ok     int
	}{{http.MethodGet, http.StatusFound}, {http.MethodPost, http.StatusSeeOther}} {
		t.Run(tc.method, func(t *testing.T) {
			h := newUIHarness(t, okBackend())
			ticket := h.ticket(h.run.ID, h.owner, oidc.RoleUser)
			v := url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {ticket}}
			browserA := h.bindCookie(ticket)

			rec := h.enterUnbound(tc.method, v)
			if rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "invalid, expired, or already-used attach ticket") {
				t.Fatalf("browser B: %d %s, want the bad-ticket 403", rec.Code, rec.Body.String())
			}
			for _, c := range rec.Result().Cookies() {
				if c.Name == uiCookieName {
					t.Fatal("browser B was given a relay session")
				}
			}
			if !h.audit.hasDataValue("reason", uiBindUnboundReason) {
				t.Fatalf("no ui.authorize/denied row for the unbound ticket: %s", h.audit.dataReasons())
			}

			if rec := h.enterUnbound(tc.method, v, browserA); rec.Code != tc.ok {
				t.Fatalf("browser A after B's attempt: %d %s, want %d", rec.Code, rec.Body.String(), tc.ok)
			}
		})
	}
}

// TestUIGateway_BindingIsSingleUseAndTicketSpecific: a binding is spent by the
// enter that uses it (the gateway clears it), a replay of it is refused, and a
// binding made for one ticket is worth nothing for another.
func TestUIGateway_BindingIsSingleUseAndTicketSpecific(t *testing.T) {
	h := newUIHarness(t, okBackend())
	v := func(ticket string) url.Values {
		return url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {ticket}}
	}

	first := h.ticket(h.run.ID, h.owner, oidc.RoleUser)
	binding := h.bindCookie(first)
	rec := h.enterUnbound(http.MethodPost, v(first), binding)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("bound enter: %d %s", rec.Code, rec.Body.String())
	}
	cleared := false
	for _, c := range rec.Result().Cookies() {
		if c.Name == binding.Name && c.MaxAge < 0 && c.Path == uiEnterPath {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("enter did not clear the binding it used: %v", rec.Header().Values("Set-Cookie"))
	}

	t.Run("replayed binding with its spent ticket", func(t *testing.T) {
		if rec := h.enterUnbound(http.MethodPost, v(first), binding); rec.Code != http.StatusForbidden {
			t.Fatalf("got %d, want 403", rec.Code)
		}
	})
	t.Run("replayed binding with a fresh ticket", func(t *testing.T) {
		fresh := h.ticket(h.run.ID, h.owner, oidc.RoleUser)
		// Under its own name and under the fresh ticket's name: the value is
		// an HMAC of the first ticket either way.
		renamed := &http.Cookie{Name: uiBindCookieName(h.srv.uiBindMAC(fresh)), Value: binding.Value}
		for _, c := range []*http.Cookie{binding, renamed} {
			if rec := h.enterUnbound(http.MethodPost, v(fresh), c); rec.Code != http.StatusForbidden {
				t.Fatalf("binding %s for another ticket: %d, want 403", c.Name, rec.Code)
			}
		}
		if rec := h.enterPOST(v(fresh)); rec.Code != http.StatusSeeOther {
			t.Fatalf("the fresh ticket, once bound: %d %s", rec.Code, rec.Body.String())
		}
	})
}

// bindRequest is one pre-enter fetch as a browser would send it.
func bindRequest(ticket string, headers map[string]string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, uiBindPath, strings.NewReader(url.Values{"ticket": {ticket}}.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req
}

// TestUIGateway_BindIsOnlyForTheConsole: an attacker's page is cross-site and
// gets no binding cookie; neither does the gateway's own (sandbox-authored)
// origin, a request with no Fetch Metadata, or — with SSO — a same-site host
// that is not the console. The console's own fetch gets an HttpOnly, Strict,
// enter-scoped cookie whose name and value carry no trace of the ticket.
func TestUIGateway_BindIsOnlyForTheConsole(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.srv.cfg.OIDCRedirectURL = "https://console.example.com/auth/callback"
	const console = "https://console.example.com"
	ticket := h.ticket(h.run.ID, h.owner, oidc.RoleUser)

	refused := map[string]*http.Request{
		"cross-site page": bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}),
		"cross-site page claiming the console origin": bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": console}),
		"the gateway's own origin":                    bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-origin", "Origin": "https://ui.example.com"}),
		"no fetch metadata":                           bindRequest(ticket, map[string]string{"Origin": console}),
		"a same-site host that is not the console":    bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "https://blog.example.com"}),
		"same-site with no Origin":                    bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-site"}),
		"the console host over the wrong scheme":      bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://console.example.com"}),
		"a ticket in the query": func() *http.Request {
			r := bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": console})
			r.URL.RawQuery = "ticket=" + ticket
			return r
		}(),
		"no ticket": bindRequest("", map[string]string{"Sec-Fetch-Site": "same-site", "Origin": console}),
		"an oversize body": bindRequest(ticket+"&pad="+strings.Repeat("x", int(maxUIEnterFormBytes)),
			map[string]string{"Sec-Fetch-Site": "same-site", "Origin": console}),
		"GET": httptest.NewRequest(http.MethodGet, uiBindPath, nil),
	}
	for name, req := range refused {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			h.gateway.ServeHTTP(rec, req)
			if rec.Code < 400 || len(rec.Result().Cookies()) != 0 || rec.Header().Get("Access-Control-Allow-Origin") != "" {
				t.Fatalf("got %d, cookies %v, ACAO %q — want a refusal with neither", rec.Code, rec.Header().Values("Set-Cookie"), rec.Header().Get("Access-Control-Allow-Origin"))
			}
		})
	}

	rec := httptest.NewRecorder()
	h.gateway.ServeHTTP(rec, bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": console}))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("console bind: %d %s", rec.Code, rec.Body.String())
	}
	if rec.Header().Get("Access-Control-Allow-Origin") != console || rec.Header().Get("Access-Control-Allow-Credentials") != "true" {
		t.Fatalf("CORS headers %v, want the console origin with credentials", rec.Header())
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("want exactly one binding cookie, got %v", rec.Header().Values("Set-Cookie"))
	}
	c := cookies[0]
	if !strings.HasPrefix(c.Name, uiBindCookiePrefix) || !c.HttpOnly || c.SameSite != http.SameSiteStrictMode ||
		c.Path != uiEnterPath || c.MaxAge <= 0 || c.MaxAge > int(attachTicketTTL.Seconds()) {
		t.Fatalf("binding cookie %+v: want wardyn_ui_bind_*, HttpOnly, Strict, Path=%s, short Max-Age", c, uiEnterPath)
	}
	if strings.Contains(rec.Header().Get("Set-Cookie"), ticket) || rec.Header().Get("Location") != "" {
		t.Fatalf("bind response leaks the ticket: %v", rec.Header())
	}

	// Host mode, SSO on: another run's app origin is same-site with this run's
	// gateway host, and must still get no binding — only the console does.
	t.Run("host mode: a sibling run's origin is not the console", func(t *testing.T) {
		h.srv.cfg.UIOriginTemplate = "https://run-{run}.ui.example.com"
		defer func() { h.srv.cfg.UIOriginTemplate = "" }()
		gw := h.srv.UIGatewayHandler()
		for origin, want := range map[string]int{
			"https://run-" + uuid.New().String() + ".ui.example.com": http.StatusForbidden,
			console: http.StatusNoContent,
		} {
			req := bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": origin})
			req.Host = h.srv.uiRunOrigin(h.run.ID)
			rec := httptest.NewRecorder()
			gw.ServeHTTP(rec, req)
			if rec.Code != want {
				t.Fatalf("Origin %s: %d %s, want %d", origin, rec.Code, rec.Body.String(), want)
			}
			if want == http.StatusForbidden && (len(rec.Result().Cookies()) != 0 || rec.Header().Get("Access-Control-Allow-Origin") != "") {
				t.Fatalf("Origin %s: refused bind still set %v / ACAO %q", origin, rec.Header().Values("Set-Cookie"), rec.Header().Get("Access-Control-Allow-Origin"))
			}
		}
	})

	t.Run("without SSO, same-site is the rule", func(t *testing.T) {
		h.srv.cfg.OIDCRedirectURL = ""
		rec := httptest.NewRecorder()
		h.gateway.ServeHTTP(rec, bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "same-site", "Origin": "http://localhost:8080"}))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("same-site bind without SSO: %d %s", rec.Code, rec.Body.String())
		}
		rec = httptest.NewRecorder()
		h.gateway.ServeHTTP(rec, bindRequest(ticket, map[string]string{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"}))
		if rec.Code != http.StatusForbidden || len(rec.Result().Cookies()) != 0 {
			t.Fatalf("cross-site bind without SSO: %d %v", rec.Code, rec.Header().Values("Set-Cookie"))
		}
	})
}

// TestUIGateway_BindRefusalIsAuditedWithItsReason: the console cannot read a
// refused bind (no CORS headers), so the gateway's audit trail is where an
// operator learns which rule refused it and what the browser sent.
func TestUIGateway_BindRefusalIsAuditedWithItsReason(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.srv.cfg.OIDCRedirectURL = "https://console.example.com/auth/callback"
	for _, hdr := range []map[string]string{
		{"Sec-Fetch-Site": "cross-site", "Origin": "https://evil.example"},
		{"Sec-Fetch-Site": "same-site", "Origin": "https://blog.example.com"},
	} {
		rec := httptest.NewRecorder()
		h.gateway.ServeHTTP(rec, bindRequest("t", hdr))
		if rec.Code != http.StatusForbidden {
			t.Fatalf("%v: %d, want 403", hdr, rec.Code)
		}
	}
	for key, want := range map[string]string{
		"reason":         uiBindReasonNotSameSite,
		"sec_fetch_site": "cross-site",
		"origin":         "https://blog.example.com",
	} {
		if !h.audit.hasDataValue(key, want) {
			t.Fatalf("no ui.authorize/denied row with %s=%q: %s", key, want, h.audit.dataReasons())
		}
	}
	if !h.audit.hasDataValue("reason", uiBindReasonOriginNotConsole) {
		t.Fatalf("no row for the non-console Origin: %s", h.audit.dataReasons())
	}
}

// TestUIGateway_HealthzPublishesBindURL: the console finds the bind step
// beside the enter endpoint, never by composing it.
func TestUIGateway_HealthzPublishesBindURL(t *testing.T) {
	h := newUIHarness(t, okBackend())
	if got, want := h.srv.uiSandboxHealthz()["bind_url"], "https://ui.example.com"+uiBindPath; got != want {
		t.Fatalf("bind_url = %v, want %s", got, want)
	}
	h.srv.cfg.UIOriginTemplate = "https://run-{run}.ui.example.com"
	if got, want := h.srv.uiSandboxHealthz()["bind_url"], "https://run-{run}.ui.example.com"+uiBindPath; got != want {
		t.Fatalf("host-mode bind_url = %v, want %s", got, want)
	}
}

// TestUIGateway_ConsoleCSPAllowsOnlyTheBindURL: the console's own CSP must let
// Open's bind fetch out (connect-src), and must name that one endpoint — not
// the gateway origin, where relayed apps answer.
func TestUIGateway_ConsoleCSPAllowsOnlyTheBindURL(t *testing.T) {
	h := newUIHarness(t, okBackend())
	for _, tc := range []struct{ name, basePath, template, want string }{
		{"path mode", "", "", " https://ui.example.com/__wardyn/bind"},
		{"base path", "/wardyn", "", " https://ui.example.com/wardyn/__wardyn/bind"},
		{"host mode", "", "https://run-{run}.ui.example.com", " https://*.ui.example.com/__wardyn/bind"},
		{"run outside the first label", "", "https://ui.{run}.example.com", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h.srv.cfg.BasePath, h.srv.cfg.UIOriginTemplate = tc.basePath, tc.template
			if got := h.srv.cspUIBindSrc(); got != tc.want {
				t.Fatalf("cspUIBindSrc = %q, want %q", got, tc.want)
			}
		})
	}
	h.srv.cfg.BasePath, h.srv.cfg.UIOriginTemplate = "", ""
	rec := httptest.NewRecorder()
	h.srv.securityHeaders(http.NotFoundHandler()).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if csp := rec.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "connect-src 'self' ws://example.com wss://example.com https://ui.example.com/__wardyn/bind;") {
		t.Fatalf("console CSP does not allow the bind fetch: %s", csp)
	}
	if got := New(Config{}).cspUIBindSrc(); got != "" {
		t.Fatalf("gateway off: cspUIBindSrc = %q, want empty", got)
	}
}

// swScopeAllowed is the browser's service-worker registration check (Service
// Workers, "Update", the max-scope steps): the requested scope's path must
// start with the max scope — Service-Worker-Allowed resolved against the
// script URL (its path only), or the script's own directory without it.
func swScopeAllowed(t *testing.T, scriptPath, allowed, scope string) bool {
	t.Helper()
	script, err := url.Parse("https://ui.example.com" + scriptPath)
	if err != nil {
		t.Fatal(err)
	}
	maxScope := script.Path[:strings.LastIndex(script.Path, "/")+1]
	if allowed != "" {
		u, err := script.Parse(allowed)
		if err != nil {
			return false
		}
		maxScope = u.Path
	}
	return strings.HasPrefix(scope, maxScope)
}

// TestUIGateway_RelayConfinesServiceWorkerScope: whatever Service-Worker-Allowed
// an app sends, the relayed worker script lets the browser register only
// inside the app's own prefix — never at "/", the enter path, another run's
// prefix, or another app of the same run. Every other relayed response loses
// the header.
func TestUIGateway_RelayConfinesServiceWorkerScope(t *testing.T) {
	for _, mode := range []struct{ name, basePath string }{{"root", ""}, {"base path", "/wardyn"}} {
		t.Run(mode.name, func(t *testing.T) {
			var appSends string
			h := newUIHarness(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/javascript")
				w.Header().Set("Service-Worker-Allowed", appSends)
				_, _ = io.WriteString(w, "self.addEventListener('fetch', () => {})")
			}))
			h.srv.cfg.BasePath = mode.basePath
			gw := h.srv.UIGatewayHandler()
			own := mode.basePath + uiRelayPrefix(h.run.ID, "code") + "/"
			other := mode.basePath + uiRelayPrefix(uuid.New(), "code") + "/"
			sibling := mode.basePath + uiRelayPrefix(h.run.ID, "code2") + "/"
			forbidden := []string{"/", mode.basePath + "/", mode.basePath + uiRunPrefix, mode.basePath + uiEnterPath, other, sibling}

			q := url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}}
			enter := httptest.NewRecorder()
			gw.ServeHTTP(enter, h.bound(httptest.NewRequest(http.MethodGet, mode.basePath+uiEnterPath+"?"+q.Encode(), nil), q))
			var sess *http.Cookie
			for _, c := range enter.Result().Cookies() {
				if c.Name == uiCookieName {
					sess = c
				}
			}
			if sess == nil {
				t.Fatalf("enter: %d %s", enter.Code, enter.Body.String())
			}

			for _, hostile := range []string{"/", "/r/", "../../../", mode.basePath + uiRunPrefix, other, "https://evil.example/"} {
				appSends = hostile
				script := own + "static/sw.js"
				req := httptest.NewRequest(http.MethodGet, script, nil)
				req.AddCookie(sess)
				req.Header.Set("Service-Worker", "script")
				rec := httptest.NewRecorder()
				gw.ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("worker script: %d %s", rec.Code, rec.Body.String())
				}
				got := rec.Header().Get("Service-Worker-Allowed")
				if got != own {
					t.Fatalf("app sent %q: relayed Service-Worker-Allowed %q, want the app's own prefix %q", hostile, got, own)
				}
				for _, scope := range forbidden {
					if swScopeAllowed(t, script, got, scope) {
						t.Fatalf("app sent %q: a worker could register at scope %q", hostile, scope)
					}
				}
				if !swScopeAllowed(t, script, got, own) {
					t.Fatalf("the app's own root %q is not registrable", own)
				}

				req = httptest.NewRequest(http.MethodGet, own+"index.html", nil)
				req.AddCookie(sess)
				rec = httptest.NewRecorder()
				gw.ServeHTTP(rec, req)
				if got := rec.Header().Values("Service-Worker-Allowed"); len(got) != 0 {
					t.Fatalf("app sent %q on a page: relayed %v, want the header removed", hostile, got)
				}
			}
		})
	}
}
