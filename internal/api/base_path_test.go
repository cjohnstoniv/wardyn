// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// testIndexHTML is the shape vite writes with base "./" (ui/vite.config.ts).
const testIndexHTML = `<!doctype html><html lang="en" class="dark" data-wardyn-base=""><head>` +
	`<link rel="icon" type="image/svg+xml" href="./favicon.svg" />` +
	`<script type="module" crossorigin src="./assets/index-abc.js"></script></head><body><div id="root"></div></body></html>`

func newBasePathServer(t *testing.T, base string) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"index.html": testIndexHTML, "assets/index-abc.js": "export {}"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	srv, _, _, _ := newAuthzMatrixServer(t, func(c *Config) { c.UIDir = dir; c.BasePath = base })
	return srv
}

func get(t *testing.T, h http.Handler, path string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	panicFails(t, h).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
	return rec
}

// TestBasePathMountsTheConsoleUnderThePrefix: with WARDYN_BASE_PATH set, the
// health probes, the API, the SPA shell (including a deep link a browser
// refreshes) and the hashed assets answer under the prefix, and the same
// paths at the host root are a 404 — the reverse proxy forwards them
// unchanged, and a neighbouring application owns the rest of the host.
func TestBasePathMountsTheConsoleUnderThePrefix(t *testing.T) {
	h := newBasePathServer(t, "/wardyn").Handler()
	for path, want := range map[string]int{
		"/wardyn/healthz":             http.StatusOK,
		"/wardyn/api/v1/runs":         http.StatusUnauthorized, // reached the API's auth, not a 404
		"/wardyn/assets/index-abc.js": http.StatusOK,
		"/wardyn":                     http.StatusOK,
		"/wardyn/":                    http.StatusOK,
		"/wardyn/runs/abc":            http.StatusOK,
		"/healthz":                    http.StatusNotFound,
		"/readyz":                     http.StatusNotFound,
		"/api/v1/runs":                http.StatusNotFound,
		"/auth/login":                 http.StatusNotFound,
		"/assets/index-abc.js":        http.StatusNotFound,
		"/":                           http.StatusNotFound,
		"/runs/abc":                   http.StatusNotFound,
		"/wardynx/healthz":            http.StatusNotFound, // a prefix of a segment is not the base
	} {
		if got := get(t, h, path).Code; got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
	// The deep link is the SPA shell, so a refresh on it boots the console.
	if body := get(t, h, "/wardyn/runs/abc").Body.String(); !strings.Contains(body, `data-wardyn-base="/wardyn"`) {
		t.Errorf("deep link did not serve the shell with the base written in: %s", body)
	}
}

// TestBasePathLeavesNoRouteAtTheRoot walks the router itself, so a route added
// later is covered without editing this test: under a base path, not one of
// them answers at the host root.
func TestBasePathLeavesNoRouteAtTheRoot(t *testing.T) {
	srv := newBasePathServer(t, "/wardyn")
	param := regexp.MustCompile(`\{[^}]+\}`)
	n := 0
	err := chi.Walk(srv.router, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		n++
		path := strings.ReplaceAll(param.ReplaceAllString(route, "x"), "/*", "/x")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(method, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s %s answered %d at the host root, want 404", method, path, rec.Code)
		}
		return nil
	})
	if err != nil || n == 0 {
		t.Fatalf("chi.Walk: %v after %d routes", err, n)
	}
}

// TestBasePathWritesTheBaseIntoIndex: the shell's "./" URLs become
// root-absolute under the base (a deep link would otherwise resolve them
// against its own directory), and data-wardyn-base hands the bundle the same
// value. Unset, the shell is exactly what a root-based build served.
func TestBasePathWritesTheBaseIntoIndex(t *testing.T) {
	for base, want := range map[string][]string{
		"/wardyn": {`data-wardyn-base="/wardyn"`, `href="/wardyn/favicon.svg"`, `src="/wardyn/assets/index-abc.js"`},
		"":        {`data-wardyn-base=""`, `href="/favicon.svg"`, `src="/assets/index-abc.js"`},
	} {
		rec := get(t, newBasePathServer(t, base).Handler(), base+"/")
		body := rec.Body.String()
		for _, w := range want {
			if !strings.Contains(body, w) {
				t.Errorf("base %q: index.html missing %s: %s", base, w, body)
			}
		}
		if strings.Contains(body, `="./`) {
			t.Errorf("base %q: index.html still carries a relative URL: %s", base, body)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") || rec.Header().Get("Cache-Control") != "no-cache" {
			t.Errorf("base %q: index.html Content-Type %q Cache-Control %q", base, ct, rec.Header().Get("Cache-Control"))
		}
	}
}

// TestBasePathUnsetIsTheRoot pins the default: nothing moves.
func TestBasePathUnsetIsTheRoot(t *testing.T) {
	srv := newBasePathServer(t, "")
	h := srv.Handler()
	for path, want := range map[string]int{"/healthz": 200, "/api/v1/runs": 401, "/runs/abc": 200, "/assets/index-abc.js": 200} {
		if got := get(t, h, path).Code; got != want {
			t.Errorf("GET %s = %d, want %d", path, got, want)
		}
	}
	if srv.cookiePath() != "/" || srv.adoCookie("n", "v").Path != "/" {
		t.Errorf("default cookie path = %q, want /", srv.cookiePath())
	}
	fallback := get(t, New(Config{}).Handler(), "/").Body.String()
	if !strings.Contains(fallback, `href="/healthz"`) || !strings.Contains(fallback, "<code>/api/v1</code>") {
		t.Errorf("fallback status page changed at the default: %s", fallback)
	}
}

// TestBasePathScopesConsoleCookiesAndRedirects: the Azure DevOps sign-in's
// one-time cookies and its return to the console stay under the base.
func TestBasePathScopesConsoleCookiesAndRedirects(t *testing.T) {
	srv := New(Config{BasePath: "/wardyn"})
	if got := srv.adoCookie("n", "v").Path; got != "/wardyn" {
		t.Errorf("ado cookie Path = %q, want /wardyn", got)
	}
	rec := httptest.NewRecorder()
	srv.clearADOCookie(rec, "n")
	if c := rec.Result().Cookies(); len(c) != 1 || c[0].Path != "/wardyn" {
		t.Errorf("cleared ado cookie = %+v, want Path /wardyn", c)
	}
	fallback := get(t, srv.Handler(), "/wardyn/").Body.String()
	if !strings.Contains(fallback, `href="/wardyn/healthz"`) {
		t.Errorf("fallback status page links outside the base: %s", fallback)
	}
}

// TestBasePathHealthzEnterTemplate: the one URL the console reads off
// /healthz to open a UI app carries the base in shared-origin path mode, and
// is the per-run origin, untouched, in host mode. Read through the real
// /healthz route, under the base.
func TestBasePathHealthzEnterTemplate(t *testing.T) {
	for origin, want := range map[string]string{
		"":                                 "https://ui.example.com/wardyn" + uiEnterPath + "?run={run}&app={app}&ticket={ticket}",
		"https://run-{run}.ui.example.com": "https://run-{run}.ui.example.com" + uiEnterPath + "?run={run}&app={app}&ticket={ticket}",
	} {
		srv, _, _, _ := newAuthzMatrixServer(t, func(c *Config) {
			c.BasePath, c.UIOriginTemplate = "/wardyn", origin
			c.UIListenAddr, c.UIAdvertiseURL, c.UISessionKey = ":8081", "https://ui.example.com", make([]byte, 32)
		})
		rec := get(t, srv.Handler(), "/wardyn/healthz")
		var body struct {
			UISandbox struct {
				EnterURLTemplate string `json:"enter_url_template"`
			} `json:"ui_sandbox"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET /wardyn/healthz = %d %v", rec.Code, err)
		}
		if body.UISandbox.EnterURLTemplate != want {
			t.Errorf("origin template %q: enter_url_template = %q, want %q", origin, body.UISandbox.EnterURLTemplate, want)
		}
	}
}

// TestBasePathUIGatewayPathMode: the shared-origin gateway serves the enter
// and relay routes under the base (and 404s them at the root), and scopes the
// relay cookie and redirect to it.
func TestBasePathUIGatewayPathMode(t *testing.T) {
	h := newUIHarness(t, okBackend())
	h.srv.cfg.BasePath = "/wardyn"
	gw := h.srv.UIGatewayHandler()

	q := url.Values{"run": {h.run.ID.String()}, "app": {"code"}, "ticket": {h.ticket(h.run.ID, h.owner, oidc.RoleUser)}}
	if rec := get(t, gw, uiEnterPath+"?"+q.Encode()); rec.Code != http.StatusNotFound {
		t.Fatalf("enter at the root = %d, want 404", rec.Code)
	}
	enter := get(t, gw, "/wardyn"+uiEnterPath+"?"+q.Encode())
	relay := "/wardyn" + uiRelayPrefix(h.run.ID, "code")
	if enter.Code != http.StatusFound || enter.Header().Get("Location") != relay+"/ide" {
		t.Fatalf("enter = %d Location %q, want 302 to %s/ide", enter.Code, enter.Header().Get("Location"), relay)
	}
	var cookie *http.Cookie
	for _, c := range enter.Result().Cookies() {
		if c.Name == uiCookieName {
			cookie = c
		}
	}
	if cookie == nil || cookie.Path != relay+"/" {
		t.Fatalf("relay cookie = %+v, want Path %s/", cookie, relay)
	}
	req := httptest.NewRequest(http.MethodGet, relay+"/ide", nil)
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	gw.ServeHTTP(rec, req)
	if body, _ := io.ReadAll(rec.Body); rec.Code != http.StatusOK || string(body) != "sandbox app" {
		t.Fatalf("relay under the base = %d %q", rec.Code, body)
	}

}
