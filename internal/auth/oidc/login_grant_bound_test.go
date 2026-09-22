// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// boundTimeout is the sink deadline these cases run under — short, so a
// stalled sink is proven harmless in milliseconds rather than the real three
// seconds.
const boundTimeout = 100 * time.Millisecond

// promptly is how long a login leg may take with a sink stalled behind it.
// Generous against the deadline so a loaded CI box cannot flake it, and far
// below "never", which is what an unbounded sink costs.
const promptly = 2 * time.Second

// blockingSink stalls the halves it is told to, IGNORING its context — the
// worst-behaved sink there is, and the one the deadline has to hold against.
type blockingSink struct {
	scopes       []string
	blockScopes  bool
	blockCapture bool
	panicCapture bool
	release      chan struct{}
}

func (b *blockingSink) LoginScopes(context.Context) []string {
	if b.blockScopes {
		<-b.release
	}
	return b.scopes
}

func (b *blockingSink) CaptureLoginGrant(context.Context, string, writoidc.LoginGrant) {
	if b.panicCapture {
		panic("a sink that breaks its contract")
	}
	if b.blockCapture {
		<-b.release
	}
}

// entraLogin is a real Authenticator against the Entra fake, so the capture
// half is reached with a real refresh token.
type entraLogin struct {
	fake *entrafake.Server
	auth *writoidc.Authenticator
}

func newEntraLogin(t *testing.T, secure bool) *entraLogin {
	t.Helper()
	fake := entrafake.New()
	t.Cleanup(fake.Close)
	redirect := "http://console.example.invalid/auth/callback"
	fake.SetRedirectURI(redirect)
	auth, err := writoidc.New(context.Background(), writoidc.Config{
		IssuerURL:     fake.Issuer(),
		ClientID:      fake.ClientID(),
		RedirectURL:   redirect,
		DefaultRole:   writoidc.RoleMember,
		SecureCookies: secure,
	}, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New against the fake tenant: %v", err)
	}
	writoidc.SetLoginGrantTimeoutForTest(auth, boundTimeout)
	return &entraLogin{fake: fake, auth: auth}
}

// run drives LoginHandler, the tenant's authorize leg and CallbackHandler, and
// returns the callback's response.
func (e *entraLogin) run(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	e.auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if w.Code != http.StatusFound {
		t.Fatalf("LoginHandler: status %d", w.Code)
	}
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Get(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	_ = resp.Body.Close()
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse the authorize redirect: %v", err)
	}
	q := loc.Query()
	params := url.Values{"state": {q.Get("state")}}
	for _, k := range []string{"code", "error", "error_description"} {
		if v := q.Get(k); v != "" {
			params.Set(k, v)
		}
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+params.Encode(), nil)
	for _, c := range w.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	e.auth.CallbackHandler(out, cb)
	return out
}

func hasSession(w *httptest.ResponseRecorder) bool {
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.Value != "" {
			return true
		}
	}
	return false
}

// TestAStalledSinkDoesNotStallTheLoginRequest: LoginHandler did no I/O before
// the seam existed, and a sink that never answers must not change that. A
// timeout means "no widening" — the request goes out as the plain login.
func TestAStalledSinkDoesNotStallTheLoginRequest(t *testing.T) {
	e := newEntraLogin(t, false)
	sink := &blockingSink{scopes: []string{"some.scope"}, blockScopes: true, release: make(chan struct{})}
	t.Cleanup(func() { close(sink.release) })
	e.auth.AttachLoginGrantSink(sink)

	start := time.Now()
	w := httptest.NewRecorder()
	e.auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	if took := time.Since(start); took > promptly {
		t.Fatalf("LoginHandler took %v behind a stalled sink; the deadline is %v", took, boundTimeout)
	}
	u, err := url.Parse(w.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse the authorization URL: %v", err)
	}
	if got := u.Query().Get("scope"); got != baseLoginScopes {
		t.Fatalf("scope = %q; a sink that did not answer must mean no widening", got)
	}
}

// TestAStalledCaptureStillYieldsASession is F1's property on the callback: the
// capture runs BEFORE the session cookie is written, so a capture that never
// returns would hold an approved human at a blank page. It must not.
func TestAStalledCaptureStillYieldsASession(t *testing.T) {
	e := newEntraLogin(t, false)
	sink := &blockingSink{
		scopes:       []string{e.fake.ConsentedScopes()[1], "offline_access"},
		blockCapture: true,
		release:      make(chan struct{}),
	}
	t.Cleanup(func() { close(sink.release) })
	e.auth.AttachLoginGrantSink(sink)

	start := time.Now()
	w := e.run(t)
	if took := time.Since(start); took > promptly {
		t.Fatalf("the login took %v behind a stalled capture; the deadline is %v", took, boundTimeout)
	}
	if !hasSession(w) {
		t.Fatalf("a stalled capture cost this person their session: status %d", w.Code)
	}
}

// TestAPanickingCaptureStillYieldsASession: on its own goroutine a sink's panic
// is no longer the request's to catch — unrecovered it would take the daemon
// down. It is recovered, and the login proceeds.
func TestAPanickingCaptureStillYieldsASession(t *testing.T) {
	e := newEntraLogin(t, false)
	e.auth.AttachLoginGrantSink(&blockingSink{
		scopes:       []string{e.fake.ConsentedScopes()[1], "offline_access"},
		panicCapture: true,
	})
	if w := e.run(t); !hasSession(w) {
		t.Fatalf("a panicking capture cost this person their session: status %d", w.Code)
	}
}

// TestAnUnwidenedCallbackWritesNoMarkerHeader pins "nothing changes without a
// row" on the CALLBACK, not only the request: a login that was never widened
// must not emit a Set-Cookie for the widened marker.
func TestAnUnwidenedCallbackWritesNoMarkerHeader(t *testing.T) {
	e := newEntraLogin(t, false)
	w := e.run(t)
	if !hasSession(w) {
		t.Fatalf("the login did not succeed: status %d", w.Code)
	}
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_oidc_widened" {
			t.Fatalf("an unwidened callback wrote a Set-Cookie for the widened marker: %+v", c)
		}
	}
}

// TestTheWidenedMarkerIsExpiredSecurely: when the marker IS cleared, the
// deletion carries the deployment's Secure posture like every other cookie
// the login writes.
func TestTheWidenedMarkerIsExpiredSecurely(t *testing.T) {
	e := newEntraLogin(t, true)
	e.auth.AttachLoginGrantSink(&blockingSink{scopes: []string{e.fake.ConsentedScopes()[1], "offline_access"}})
	w := e.run(t)
	if !hasSession(w) {
		t.Fatalf("the login did not succeed: status %d", w.Code)
	}
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name != "wardyn_oidc_widened" {
			continue
		}
		found = true
		if !c.Secure || c.MaxAge >= 0 {
			t.Errorf("the marker deletion is Secure=%v MaxAge=%d; want Secure and expired", c.Secure, c.MaxAge)
		}
	}
	if !found {
		t.Fatal("a widened login's callback did not expire the marker")
	}
}

// TestAnUnretriedRefusalIsLogged is F2: a refusal outside the retry set still
// answers as it always has, but its code and description reach the log.
func TestAnUnretriedRefusalIsLogged(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	e := newEntraLogin(t, false)
	w := httptest.NewRecorder()
	e.auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login", nil))
	u, _ := url.Parse(w.Header().Get("Location"))
	q := url.Values{
		"state":             {u.Query().Get("state")},
		"error":             {"temporarily_unavailable"},
		"error_description": {"AADSTS90033: a transient error has occurred"},
	}
	cb := httptest.NewRequest(http.MethodGet, "/auth/callback?"+q.Encode(), nil)
	for _, c := range w.Result().Cookies() {
		cb.AddCookie(c)
	}
	out := httptest.NewRecorder()
	e.auth.CallbackHandler(out, cb)
	if out.Code != http.StatusBadRequest {
		t.Fatalf("status %d; the response to an unretried refusal must be unchanged (400)", out.Code)
	}
	logged := buf.String()
	if !strings.Contains(logged, "temporarily_unavailable") || !strings.Contains(logged, "AADSTS90033") {
		t.Fatalf("the refusal's code and description are not in the log: %q", logged)
	}
}
