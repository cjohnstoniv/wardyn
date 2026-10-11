// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// rewriteSeen runs one request through a split-horizon rewrite transport and
// returns the URL the base transport received.
func rewriteSeen(t *testing.T, publicURL, internalURL, reqURL string) string {
	t.Helper()
	var saw string
	base := roundTripFunc(func(r *http.Request) (*http.Response, error) {
		saw = r.URL.Scheme + "://" + r.URL.Host + r.URL.Path
		return &http.Response{StatusCode: 200, Body: http.NoBody, Header: make(http.Header)}, nil
	})
	rt, err := writoidc.NewRewriteTransportForTest(publicURL, internalURL, &http.Client{Transport: base})
	if err != nil {
		t.Fatalf("newRewriteTransport: %v", err)
	}
	req, _ := http.NewRequest(http.MethodGet, reqURL, nil)
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if req.URL.String() != reqURL {
		t.Errorf("caller request mutated: %q, want %q", req.URL.String(), reqURL)
	}
	return saw
}

// Characterization: a same-scheme pairing keeps its scheme, and a request to
// any other host passes through with its own scheme and host.
func TestRewriteTransportSameSchemeAndOtherHosts(t *testing.T) {
	if got := rewriteSeen(t, "https://idp.example.com/dex", "https://dex.idp.svc:5556/dex", "https://idp.example.com/dex/token"); got != "https://dex.idp.svc:5556/dex/token" {
		t.Errorf("https/https rewrite: base saw %q", got)
	}
	if got := rewriteSeen(t, "https://idp.example.com/dex", "http://dex.idp.svc:5556/dex", "https://api.example.org/v1"); got != "https://api.example.org/v1" {
		t.Errorf("unrelated host rewritten: base saw %q", got)
	}
}

// An https public issuer paired with a plain-http internal one (the mesh
// opt-out) must reach the internal listener over http: the token and JWKS
// endpoints come from the discovery document with the public scheme.
func TestRewriteTransportUsesInternalScheme(t *testing.T) {
	for _, path := range []string{"/dex/token", "/dex/keys"} {
		if got := rewriteSeen(t, "https://idp.example.com/dex", "http://dex.idp.svc:5556/dex", "https://idp.example.com"+path); got != "http://dex.idp.svc:5556"+path {
			t.Errorf("%s: base saw %q, want http://dex.idp.svc:5556%s", path, got, path)
		}
	}
}

// Characterization: a no_role refusal still redirects with auth_error=no_role,
// issues no session and clears any existing one.
func TestNoRoleRefusalRedirect(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleUser}, "", nil)
	env.buildIDTokenWithRoles(t, "sub-norole", "dave@corp.example", nil, nil, roleCallbackNonce, time.Now().Add(time.Hour))
	w, sess := doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(nil, func(*http.Request, string) {}))
	if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, writoidc.DenialNoRole) {
		t.Errorf("Location = %q, want auth_error=no_role", loc)
	}
	if sess.Role != "" {
		t.Errorf("role = %q, want no session issued", sess.Role)
	}
	assertSessionCookieCleared(t, w)
}

// A no_role refusal is reported to onDenied and leaves a log line naming the
// subject, like the other sign-in refusals.
func TestNoRoleRefusalIsLoggedAndReported(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	env := newIdPEnv(t)
	auth := env.newRoleAuth(t, map[string]string{"eng-team": writoidc.RoleUser}, "", nil)
	env.buildIDTokenWithRoles(t, "sub-norole", "dave@corp.example", nil, nil, roleCallbackNonce, time.Now().Add(time.Hour))
	var denied []string
	doCallbackVia(t, auth, auth.CallbackHandlerWithDenials(nil, func(_ *http.Request, reason string) { denied = append(denied, reason) }))

	if len(denied) != 1 || denied[0] != writoidc.DenialNoRole {
		t.Errorf("onDenied reasons = %v, want [no_role]", denied)
	}
	logged := buf.String()
	if !strings.Contains(logged, "reason=no_role") || !strings.Contains(logged, "sub=sub-norole") {
		t.Errorf("no log line for the no_role refusal; log:\n%s", logged)
	}
}

// hangRT blocks every request whose path ends in suffix until the request's
// own context ends (or the test releases it); every other request goes to base.
type hangRT struct {
	suffix  string
	base    http.RoundTripper
	release chan struct{}
	once    sync.Once
}

func (h *hangRT) unblock() { h.once.Do(func() { close(h.release) }) }

func (h *hangRT) RoundTrip(r *http.Request) (*http.Response, error) {
	if !strings.HasSuffix(r.URL.Path, h.suffix) {
		return h.base.RoundTrip(r)
	}
	select {
	case <-r.Context().Done():
		return nil, r.Context().Err()
	case <-h.release:
		return nil, errors.New("released by test cleanup")
	}
}

// An IdP endpoint that accepts the request but never answers must not hold
// the callback open: the token exchange and the JWKS fetch each give up after
// the IdP request timeout. The callback's own request has no deadline here,
// so without that timeout it would block until the test releases it.
func TestIdPRequestsAreBounded(t *testing.T) {
	defer writoidc.SetIDPHTTPTimeoutForTest(50 * time.Millisecond)()

	for _, c := range []struct {
		name, hangPath, wantAuthError string
	}{
		{name: "token exchange", hangPath: "/token", wantAuthError: "oidc_transient"},
		{name: "jwks fetch", hangPath: "/keys"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			hang := &hangRT{suffix: c.hangPath, release: make(chan struct{}), base: &rewriteTokenRT{
				base:          http.DefaultTransport,
				originalToken: env.httpSrv.URL + "/token",
				replacedToken: env.tokenSrv.URL + "/",
			}}
			t.Cleanup(hang.unblock)
			ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: hang})
			auth, err := writoidc.New(ctx, writoidc.Config{
				IssuerURL:    env.httpSrv.URL,
				ClientID:     env.clientID,
				ClientSecret: "secret",
				RedirectURL:  "http://localhost/auth/callback",
			}, testHMACKey)
			if err != nil {
				t.Fatalf("writoidc.New: %v", err)
			}
			env.buildIDToken(t, "sub-hang", "hang@example.com", roleCallbackNonce, time.Now().Add(time.Hour))

			r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s&code=testcode", nil)
			r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: "s"})
			r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: roleCallbackNonce})
			r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: "v"})
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				auth.CallbackHandler(w, r)
				done <- w
			}()
			select {
			case w := <-done:
				if c.wantAuthError != "" {
					if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, c.wantAuthError) {
						t.Errorf("Location = %q, want auth_error=%s", loc, c.wantAuthError)
					}
				}
			case <-time.After(10 * time.Second):
				t.Errorf("callback still blocked on a hung %s after 10s", c.hangPath)
				hang.unblock()
				<-done
			}
		})
	}
}
