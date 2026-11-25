// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"bytes"
	"context"
	"errors"
	"io"
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

const (
	discoveryPath = "/.well-known/openid-configuration"
	keysPath      = "/keys"
)

type chaosRule func(req *http.Request, base http.RoundTripper) (*http.Response, error)

// chaosRT is the injected HTTP transport of the pending tests: per request path, it can refuse the connection,
// answer a status, hang, or serve its own body, and it counts every request.
type chaosRT struct {
	base  http.RoundTripper
	mu    sync.Mutex
	rules map[string]chaosRule
	hits  map[string]int
}

func newChaos(env *idpEnv) *chaosRT {
	return &chaosRT{
		base:  &rewriteTokenRT{base: http.DefaultTransport, originalToken: env.httpSrv.URL + "/token", replacedToken: env.tokenSrv.URL + "/"},
		rules: map[string]chaosRule{}, hits: map[string]int{},
	}
}

func (c *chaosRT) set(path string, r chaosRule) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if r == nil {
		delete(c.rules, path)
		return
	}
	c.rules[path] = r
}

func (c *chaosRT) count(path string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.hits[path]
}

func (c *chaosRT) total() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, v := range c.hits {
		n += v
	}
	return n
}

func (c *chaosRT) RoundTrip(req *http.Request) (*http.Response, error) {
	c.mu.Lock()
	c.hits[req.URL.Path]++
	rule := c.rules[req.URL.Path]
	c.mu.Unlock()
	if rule != nil {
		return rule(req, c.base)
	}
	return c.base.RoundTrip(req)
}

func refuseRule(*http.Request, http.RoundTripper) (*http.Response, error) {
	return nil, errors.New("dial tcp: connection refused")
}

func hangRule(req *http.Request, _ http.RoundTripper) (*http.Response, error) {
	<-req.Context().Done()
	return nil, req.Context().Err()
}

func statusRule(code int) chaosRule {
	return func(req *http.Request, _ http.RoundTripper) (*http.Response, error) {
		return &http.Response{StatusCode: code, Status: http.StatusText(code), Body: io.NopCloser(strings.NewReader("no")), Header: http.Header{}, Request: req}, nil
	}
}

func bodyRule(body string) chaosRule {
	return func(req *http.Request, _ http.RoundTripper) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Status: "OK", Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": {"application/json"}}, Request: req}, nil
	}
}

// pendingEnv is an authenticator for a known issuer whose provider does not answer yet.
type pendingEnv struct {
	env   *idpEnv
	chaos *chaosRT
	rev   *cutLedger
	auth  *writoidc.Authenticator
	ctx   context.Context
	sink  *recSink
}

func newPendingEnv(t *testing.T, opts ...func(*writoidc.Config)) *pendingEnv {
	t.Helper()
	env := newIdPEnv(t)
	return newPendingEnvFor(t, env, &cutLedger{seq: 10}, opts...)
}

func newPendingEnvFor(t *testing.T, env *idpEnv, rev *cutLedger, opts ...func(*writoidc.Config)) *pendingEnv {
	t.Helper()
	chaos := newChaos(env)
	chaos.set(discoveryPath, refuseRule)
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: chaos})
	cfg := writoidc.Config{
		IssuerURL: env.httpSrv.URL, ClientID: env.clientID, ClientSecret: "secret",
		RedirectURL: "http://localhost/auth/callback", Revocations: rev, BackchannelLogout: true,
	}
	for _, o := range opts {
		o(&cfg)
	}
	auth, err := writoidc.NewPending(cfg, testHMACKey)
	if err != nil {
		t.Fatalf("NewPending: %v", err)
	}
	if err := auth.Discover(ctx); !errors.Is(err, writoidc.ErrProviderUnreachable) {
		t.Fatalf("Discover against a refused provider = %v, want ErrProviderUnreachable", err)
	}
	if !auth.Pending() {
		t.Fatal("an authenticator whose provider refused the connection is not pending")
	}
	p := &pendingEnv{env: env, chaos: chaos, rev: rev, auth: auth, ctx: ctx, sink: &recSink{rev: rev}}
	auth.AttachLogoutQueueSink(p.sink)
	return p
}

// recover makes the provider answer.
func (p *pendingEnv) recover() { p.chaos.set(discoveryPath, nil) }

// recSink records the queue's events and applies a verified held logout to rev.
type recSink struct {
	mu       sync.Mutex
	rev      writoidc.SessionRevocations
	events   []writoidc.LogoutQueueEvent
	applied  []string
	applyErr error
	onApply  func()
	// denyCharge makes ChargeLogout refuse; charges counts its calls and lateCharges those for a late held logout.
	denyCharge           bool
	charges, lateCharges int
}

func (s *recSink) ChargeLogout(ctx context.Context) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.charges++
	if writoidc.LogoutLate(ctx) {
		s.lateCharges++
	}
	return !s.denyCharge
}

func (s *recSink) Noted(_ context.Context, ev writoidc.LogoutQueueEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, ev)
}

func (s *recSink) Apply(ctx context.Context, sub string, _ time.Time, sha string) error {
	s.mu.Lock()
	err, hook := s.applyErr, s.onApply
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	if err != nil {
		return err
	}
	if err := writoidc.CutSessions(ctx, s.rev, sub); err != nil {
		return err
	}
	s.mu.Lock()
	s.applied = append(s.applied, sub+":"+sha)
	s.mu.Unlock()
	return nil
}

func (s *recSink) states() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, e := range s.events {
		out = append(out, e.State)
	}
	return out
}

func (s *recSink) count(state string) int {
	n := 0
	for _, st := range s.states() {
		if st == state {
			n++
		}
	}
	return n
}

// captureLogs sends slog to a buffer for the test.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&lockedWriter{w: &buf}, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

type lockedWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

// P1: a pending authenticator starts no sign-in: 503, Retry-After, no cookie, no request to the provider.
func TestPending_LoginRefusedNoCookies(t *testing.T) {
	p := newPendingEnv(t)
	before := p.chaos.total()
	w := httptest.NewRecorder()
	p.auth.LoginHandler(w, httptest.NewRequest(http.MethodGet, "/auth/login?return_to=/runs", nil))
	if w.Code != http.StatusServiceUnavailable || w.Header().Get("Retry-After") == "" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("login = %d, Retry-After %q, Cache-Control %q; want 503 with both", w.Code, w.Header().Get("Retry-After"), w.Header().Get("Cache-Control"))
	}
	if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("a refused sign-in set cookies: %v", got)
	}
	if n := p.chaos.total() - before; n != 0 {
		t.Errorf("a refused sign-in made %d provider requests, want 0", n)
	}
}

// P2: the callback while pending is a 503 that consumes no cookie and makes no exchange.
func TestPending_CallbackRefusedCookiesKept(t *testing.T) {
	p := newPendingEnv(t)
	before := p.chaos.total()
	r := httptest.NewRequest(http.MethodGet, "/auth/callback?state=s&code=c", nil)
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_state", Value: "s"})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_nonce", Value: "n"})
	r.AddCookie(&http.Cookie{Name: "wardyn_oidc_pkce", Value: "v"})
	w := httptest.NewRecorder()
	p.auth.CallbackHandler(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Errorf("callback = %d, want 503", w.Code)
	}
	if got := w.Header().Values("Set-Cookie"); len(got) != 0 {
		t.Errorf("the refused callback consumed or set cookies: %v", got)
	}
	if n := p.chaos.total() - before; n != 0 {
		t.Errorf("the refused callback made %d provider requests, want 0", n)
	}
}

// The subject and actor token checks refuse while pending, with their existing reasons.
func TestPending_SubjectAndActorTokensInvalid(t *testing.T) {
	p := newPendingEnv(t)
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	if _, denied := p.auth.VerifySubjectToken(r, "x.y.z", "portal", nil); denied != writoidc.SubjectTokenInvalid {
		t.Errorf("subject token while pending = %q, want %q", denied, writoidc.SubjectTokenInvalid)
	}
	if _, denied := p.auth.VerifyActorToken(r, "x.y.z", "portal", writoidc.Session{}); denied != writoidc.ActorTokenInvalid {
		t.Errorf("actor token while pending = %q, want %q", denied, writoidc.ActorTokenInvalid)
	}
}

// P15: a cookie minted by a discovered authenticator is accepted while pending, and one past a cutoff is
// rejected by both.
func TestPending_MiddlewareParity(t *testing.T) {
	env := newIdPEnv(t)
	rev := &cutLedger{seq: 10}
	disc := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) { cfg.Revocations = rev })
	cookie := signedInCookie(t, env, disc)
	p := newPendingEnvFor(t, env, rev)
	for name, a := range map[string]*writoidc.Authenticator{"discovered": disc, "pending": p.auth} {
		if live, reason := replay(a, cookie); !live {
			t.Errorf("%s: a fresh cookie was refused: %q", name, reason)
		}
	}
	if err := writoidc.CutSessions(context.Background(), rev, "pairwise-1"); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string]*writoidc.Authenticator{"discovered": disc, "pending": p.auth} {
		if live, reason := replay(a, cookie); live || reason != "revoked_session" {
			t.Errorf("%s: a cookie from before the cut = live %v, %q; want revoked_session", name, live, reason)
		}
	}
}

// P16: the zero value keeps its v0.10.2 behaviour: not pending, no stats, not closed.
func TestZeroValueAuthenticator_NotPending(t *testing.T) {
	a := &writoidc.Authenticator{}
	if a.Pending() {
		t.Error("the zero value reports pending")
	}
	if got := a.LogoutQueueStats(); got != (writoidc.LogoutQueueStats{}) {
		t.Errorf("the zero value's queue stats = %+v, want zeros", got)
	}
	if writoidc.AuthenticatorClosedForTest(a) {
		t.Error("the zero value is closed: its sign-in would answer 503 as it never did")
	}
}

// P16: only an unreachable provider makes an authenticator pending.
func TestPending_SetOnlyByUnreachable(t *testing.T) {
	env := newIdPEnv(t)
	chaos := newChaos(env)
	ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: chaos})
	cfg := writoidc.Config{IssuerURL: env.httpSrv.URL, ClientID: env.clientID, RedirectURL: "http://localhost/auth/callback"}
	a, err := writoidc.NewPending(cfg, testHMACKey)
	if err != nil {
		t.Fatal(err)
	}
	if a.Pending() {
		t.Error("an authenticator whose Discover has not run is pending")
	}
	chaos.set(discoveryPath, statusRule(404))
	if err := a.Discover(ctx); err == nil || errors.Is(err, writoidc.ErrProviderUnreachable) || a.Pending() {
		t.Errorf("a 404 discovery = %v, pending %v; want a plain error and not pending", err, a.Pending())
	}
	chaos.set(discoveryPath, statusRule(503))
	if err := a.Discover(ctx); !errors.Is(err, writoidc.ErrProviderUnreachable) || !a.Pending() {
		t.Errorf("a 503 discovery = %v, pending %v; want unreachable and pending", err, a.Pending())
	}
	chaos.set(discoveryPath, nil)
	if err := a.Discover(ctx); err != nil || a.Pending() {
		t.Errorf("a recovered provider = %v, pending %v; want nil and not pending", err, a.Pending())
	}
	if err := a.Discover(ctx); err != nil {
		t.Errorf("a second Discover after the publish = %v, want nil", err)
	}
}

// P14: NewPending refuses what New refuses, with the same errors.
func TestNewPending_StaticValidation(t *testing.T) {
	env := newIdPEnv(t)
	good := writoidc.Config{IssuerURL: env.httpSrv.URL, ClientID: "c", RedirectURL: "http://x/auth/callback"}
	cases := map[string]struct {
		cfg writoidc.Config
		key []byte
	}{
		"no issuer":      {writoidc.Config{ClientID: "c", RedirectURL: "r"}, testHMACKey},
		"no client":      {writoidc.Config{IssuerURL: "i", RedirectURL: "r"}, testHMACKey},
		"no redirect":    {writoidc.Config{IssuerURL: "i", ClientID: "c"}, testHMACKey},
		"a short hmac":   {good, []byte("short")},
		"an empty hmac":  {good, nil},
		"all of the lot": {writoidc.Config{}, nil},
	}
	for name, c := range cases {
		_, perr := writoidc.NewPending(c.cfg, c.key)
		_, nerr := writoidc.New(context.Background(), c.cfg, c.key)
		if perr == nil || nerr == nil || perr.Error() != nerr.Error() {
			t.Errorf("%s: NewPending = %v, New = %v; want the same error", name, perr, nerr)
		}
	}
}

// P12: a provider that gave no answer, or answered 5xx or 429, is the unreachable class.
func TestDiscover_UnreachableClass(t *testing.T) {
	env := newIdPEnv(t)
	cases := map[string]struct {
		rule    chaosRule
		timeout time.Duration
	}{
		"dial refused": {rule: refuseRule},
		"a timeout":    {rule: hangRule, timeout: 150 * time.Millisecond},
		"a 503":        {rule: statusRule(503)},
		"a 429":        {rule: statusRule(429)},
		"a 500":        {rule: statusRule(500)},
	}
	for name, c := range cases {
		chaos := newChaos(env)
		chaos.set(discoveryPath, c.rule)
		ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: chaos})
		if c.timeout > 0 {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, c.timeout)
			defer cancel()
		}
		_, err := writoidc.New(ctx, writoidc.Config{IssuerURL: env.httpSrv.URL, ClientID: env.clientID, RedirectURL: "http://localhost/auth/callback"}, testHMACKey)
		if !errors.Is(err, writoidc.ErrProviderUnreachable) {
			t.Errorf("%s: err = %v, want ErrProviderUnreachable", name, err)
		}
	}
}

// P13: an answer that says the configuration is wrong is not the unreachable class: a 404, an issuer mismatch,
// a bad document, an extra scope the provider does not advertise, and a transport that cannot be built.
func TestDiscover_ConfigErrorsNotUnreachable(t *testing.T) {
	env := newIdPEnv(t)
	cfg := writoidc.Config{IssuerURL: env.httpSrv.URL, ClientID: env.clientID, RedirectURL: "http://localhost/auth/callback"}
	scopes := func(req *http.Request, base http.RoundTripper) (*http.Response, error) {
		return bodyRule(`{"issuer":"`+env.httpSrv.URL+`","authorization_endpoint":"`+env.httpSrv.URL+`/auth","token_endpoint":"`+env.httpSrv.URL+
			`/token","jwks_uri":"`+env.httpSrv.URL+`/keys","scopes_supported":["openid","email"]}`)(req, base)
	}
	cases := map[string]struct {
		rule chaosRule
		edit func(*writoidc.Config)
	}{
		"a 404":                 {rule: statusRule(404)},
		"a 403":                 {rule: statusRule(403)},
		"an issuer mismatch":    {rule: bodyRule(`{"issuer":"https://other.example","jwks_uri":"https://other.example/keys"}`)},
		"a non-JSON 200":        {rule: bodyRule(`<html>nope`)},
		"an unadvertised scope": {rule: scopes, edit: func(c *writoidc.Config) { c.ExtraScopes = []string{"groups"} }},
		"a bad internal URL":    {edit: func(c *writoidc.Config) { c.InternalIssuerURL = "http://" }},
	}
	for name, c := range cases {
		chaos := newChaos(env)
		if c.rule != nil {
			chaos.set(discoveryPath, c.rule)
		}
		ctx := gooidc.ClientContext(context.Background(), &http.Client{Transport: chaos})
		cc := cfg
		if c.edit != nil {
			c.edit(&cc)
		}
		a, err := writoidc.NewPending(cc, testHMACKey)
		if err != nil {
			t.Fatalf("%s: NewPending: %v", name, err)
		}
		err = a.Discover(ctx)
		if err == nil || errors.Is(err, writoidc.ErrProviderUnreachable) || a.Pending() {
			t.Errorf("%s: Discover = %v, pending %v; want a plain error, not the unreachable class", name, err, a.Pending())
		}
	}
}

func gooidcCtx(c *chaosRT) context.Context {
	return gooidc.ClientContext(context.Background(), &http.Client{Transport: c})
}
