// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// flipRT is the injected HTTP transport of these tests: per request path it can answer a status in place of the
// provider, or hang until the request's context ends.
type flipRT struct {
	mu     sync.Mutex
	status map[string]int // path -> status answered instead of the provider; -1 hangs
}

func (f *flipRT) set(path string, status int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.status == nil {
		f.status = map[string]int{}
	}
	if status == 0 {
		delete(f.status, path)
		return
	}
	f.status[path] = status
}

func (f *flipRT) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	st := f.status[req.URL.Path]
	f.mu.Unlock()
	switch {
	case st == -1:
		<-req.Context().Done()
		return nil, req.Context().Err()
	case st > 0:
		return &http.Response{StatusCode: st, Status: http.StatusText(st), Body: http.NoBody, Header: http.Header{}, Request: req}, nil
	}
	return http.DefaultTransport.RoundTrip(req)
}

type pendingIdP struct {
	srv  *httptest.Server
	priv *rsa.PrivateKey
	rt   *flipRT
	ctx  context.Context
}

const (
	pendingDiscoveryPath = "/.well-known/openid-configuration"
	pendingKeysPath      = "/keys"
	pendingClient        = "test-client"
)

func newPendingIdP(t *testing.T) *pendingIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &oidctest.Server{PublicKeys: []oidctest.PublicKey{{PublicKey: priv.Public(), KeyID: "test-key", Algorithm: "RS256"}}}
	srv := httptest.NewServer(idp)
	t.Cleanup(srv.Close)
	idp.SetIssuer(srv.URL)
	p := &pendingIdP{srv: srv, priv: priv, rt: &flipRT{}}
	p.ctx = gooidc.ClientContext(context.Background(), &http.Client{Transport: p.rt})
	return p
}

func (p *pendingIdP) config() oidc.Config {
	return oidc.Config{IssuerURL: p.srv.URL, ClientID: pendingClient, ClientSecret: "s", RedirectURL: "http://localhost/auth/callback", BackchannelLogout: true}
}

func known(v bool) issuerKnown {
	return func(context.Context, string) (bool, error) { return v, nil }
}

var pendingKey = []byte("01234567890123456789012345678901")

// P10: an issuer the database does not know, a failed read and a nil pool all refuse boot as before.
func TestConsoleAuthenticator_UnknownIssuerRefuses(t *testing.T) {
	p := newPendingIdP(t)
	p.rt.set(pendingDiscoveryPath, 503)
	for name, k := range map[string]issuerKnown{
		"unknown":    known(false),
		"read error": func(context.Context, string) (bool, error) { return false, errors.New("db down") },
		"nil pool":   consoleIssuerKnown(nil),
	} {
		a, err := consoleAuthenticator(p.ctx, p.ctx, k, p.config(), pendingKey)
		if err == nil || a != nil {
			t.Errorf("%s: a provider that does not answer booted (authenticator %v, error %v), want today's refusal", name, a, err)
		}
	}
}

// P10: a failed console_issuers read is unknown, never an error of its own.
func TestConsoleIssuerKnown_ReadErrorIsUnknown(t *testing.T) {
	p := newPendingIdP(t)
	a, err := consoleAuthenticator(p.ctx, p.ctx, func(context.Context, string) (bool, error) { return true, errors.New("read failed") }, p.config(), pendingKey)
	if err != nil || a == nil || a.Pending() {
		t.Fatalf("a failed read with a healthy provider = %v, %v (pending %v); want a normal boot", a, err, a != nil && a.Pending())
	}
	if got, err := consoleIssuerKnown(nil)(context.Background(), "https://idp.example"); got || err != nil {
		t.Errorf("a nil pool knows %v (%v), want false, nil", got, err)
	}
}

// P12: on a known issuer, a provider that answers 503 or 429 or refuses the connection boots pending.
func TestConsoleAuthenticator_KnownIssuerUnreachableBootsPending(t *testing.T) {
	for name, status := range map[string]int{"503": 503, "429": 429, "500": 500} {
		t.Run(name, func(t *testing.T) {
			p := newPendingIdP(t)
			p.rt.set(pendingDiscoveryPath, status)
			rootCtx, cancel := context.WithCancel(p.ctx)
			defer cancel()
			a, err := consoleAuthenticator(rootCtx, p.ctx, known(true), p.config(), pendingKey)
			if err != nil || a == nil || !a.Pending() {
				t.Fatalf("known issuer, provider %d = %v, %v; want a pending authenticator", status, a, err)
			}
		})
	}
	t.Run("refused", func(t *testing.T) {
		p := newPendingIdP(t)
		cfg := p.config()
		p.srv.Close() // the connection is refused
		rootCtx, cancel := context.WithCancel(context.Background())
		defer cancel()
		a, err := consoleAuthenticator(rootCtx, context.Background(), known(true), cfg, pendingKey)
		if err != nil || a == nil || !a.Pending() {
			t.Fatalf("known issuer, connection refused = %v, %v; want a pending authenticator", a, err)
		}
	})
}

// P13: on a known issuer, a configuration problem refuses boot: a 404, a bad document, a transport that cannot be built.
func TestConsoleAuthenticator_KnownIssuerConfigErrorRefusesBoot(t *testing.T) {
	for name, edit := range map[string]func(*pendingIdP, *oidc.Config){
		"404":              func(p *pendingIdP, _ *oidc.Config) { p.rt.set(pendingDiscoveryPath, 404) },
		"403":              func(p *pendingIdP, _ *oidc.Config) { p.rt.set(pendingDiscoveryPath, 403) },
		"bad internal URL": func(_ *pendingIdP, c *oidc.Config) { c.InternalIssuerURL = "http://" },
	} {
		t.Run(name, func(t *testing.T) {
			p := newPendingIdP(t)
			cfg := p.config()
			edit(p, &cfg)
			rootCtx, cancel := context.WithCancel(p.ctx)
			defer cancel()
			a, err := consoleAuthenticator(rootCtx, p.ctx, known(true), cfg, pendingKey)
			if err == nil || a != nil {
				t.Fatalf("a configuration error on a known issuer booted (%v, %v), want the refusal", a, err)
			}
			if errors.Is(err, oidc.ErrProviderUnreachable) {
				t.Errorf("a configuration error wrapped ErrProviderUnreachable: %v", err)
			}
		})
	}
}

// P11: a provider that hangs gives a pending authenticator inside the first-attempt budget and leaves bootCtx live.
func TestConsoleAuthenticator_HangBounded(t *testing.T) {
	old := firstDiscoverBudget
	firstDiscoverBudget = 300 * time.Millisecond
	t.Cleanup(func() { firstDiscoverBudget = old })
	p := newPendingIdP(t)
	p.rt.set(pendingDiscoveryPath, -1)
	rootCtx, cancelRoot := context.WithCancel(p.ctx)
	defer cancelRoot()
	bootCtx, cancelBoot := context.WithTimeout(p.ctx, 30*time.Second)
	defer cancelBoot()
	start := time.Now()
	a, err := consoleAuthenticator(rootCtx, bootCtx, known(true), p.config(), pendingKey)
	if err != nil || a == nil || !a.Pending() {
		t.Fatalf("a hanging provider on a known issuer = %v, %v; want pending", a, err)
	}
	if d := time.Since(start); d > firstDiscoverBudget+time.Second {
		t.Errorf("boot waited %v for a hanging provider, want within %v", d, firstDiscoverBudget+time.Second)
	}
	if bootCtx.Err() != nil {
		t.Errorf("the first attempt used up bootCtx: %v", bootCtx.Err())
	}
}

type testRevocations struct {
	mu   sync.Mutex
	cuts []string
}

func (r *testRevocations) IsSessionRevoked(context.Context, string, string, time.Time) (bool, error) {
	return false, nil
}
func (r *testRevocations) RevokeSub(context.Context, string) error { return nil }
func (r *testRevocations) RevokeAll(context.Context) error         { return nil }
func (r *testRevocations) CutSessions(_ context.Context, sub string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cuts = append(r.cuts, sub)
	return nil
}

// pendingWithHeldLogout is a pending authenticator for p holding one genuine logout for sub, posted through
// its own handler.
func pendingWithHeldLogout(t *testing.T, p *pendingIdP, sub string) (*oidc.Authenticator, *testRevocations) {
	t.Helper()
	p.rt.set(pendingDiscoveryPath, 503)
	rev := &testRevocations{}
	cfg := p.config()
	cfg.Revocations = rev
	a, err := oidc.NewPending(cfg, pendingKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Discover(p.ctx); !errors.Is(err, oidc.ErrProviderUnreachable) {
		t.Fatalf("Discover = %v, want unreachable", err)
	}
	claims, _ := json.Marshal(map[string]any{
		"iss": p.srv.URL, "aud": pendingClient, "sub": sub, "jti": "j1", "iat": time.Now().Unix(), "exp": time.Now().Add(2 * time.Minute).Unix(),
		"events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}},
	})
	form := url.Values{"logout_token": {oidctest.SignIDToken(p.priv, "test-key", "RS256", string(claims))}}
	r := httptest.NewRequest(http.MethodPost, "/auth/backchannel-logout", strings.NewReader(form.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	a.BackchannelLogoutHandler(nil, nil)(w, r)
	if w.Code != http.StatusServiceUnavailable || a.LogoutQueueStats().Held != 1 {
		t.Fatalf("the logout = %d, held %d; want it held with 503", w.Code, a.LogoutQueueStats().Held)
	}
	return a, rev
}

// P11: discoverUntilReady recovers once the provider answers, and stops on cancel.
func TestDiscoverUntilReady_RecoversAndStops(t *testing.T) {
	p := newPendingIdP(t)
	a, _ := pendingWithHeldLogout(t, p, "pairwise-1")
	var waits []time.Duration
	discoverUntilReady(p.ctx, a, func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 2 {
			p.rt.set(pendingDiscoveryPath, 0) // the provider answers
		}
		return true
	})
	if a.Pending() || a.LogoutQueueStats().Held != 0 || a.LogoutQueueStats().Applied != 1 {
		t.Errorf("after recovery: pending %v, stats %+v; want published with the held logout applied", a.Pending(), a.LogoutQueueStats())
	}
	if len(waits) != 2 || waits[0] != 2*time.Second || waits[1] != 4*time.Second {
		t.Errorf("waits %v, want [2s 4s]", waits)
	}

	// Stopping while a logout is still held logs it at ERROR.
	p2 := newPendingIdP(t)
	b, _ := pendingWithHeldLogout(t, p2, "pairwise-2")
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	ctx, cancel := context.WithCancel(p2.ctx)
	discoverUntilReady(ctx, b, func(context.Context, time.Duration) bool { cancel(); return false })
	if !strings.Contains(buf.String(), "was never applied") {
		t.Errorf("a stop with a held logout logged no ERROR line:\n%s", buf.String())
	}
}

// P5b: a key endpoint that does not answer takes the backoff, 2 s doubling to the 60 s cap; the attempt after it
// recovers applies the held logout and publishes.
func TestDiscoverUntilReady_KeyFetchBacksOff(t *testing.T) {
	p := newPendingIdP(t)
	a, rev := pendingWithHeldLogout(t, p, "pairwise-1")
	p.rt.set(pendingDiscoveryPath, 0)
	p.rt.set(pendingKeysPath, 503)
	var waits []time.Duration
	discoverUntilReady(p.ctx, a, func(_ context.Context, d time.Duration) bool {
		waits = append(waits, d)
		if len(waits) == 7 {
			p.rt.set(pendingKeysPath, 0)
		}
		return true
	})
	want := []time.Duration{2, 4, 8, 16, 32, 60, 60}
	for i, w := range want {
		if i >= len(waits) || waits[i] != w*time.Second {
			t.Fatalf("waits %v, want 2s doubling to the 60s cap", waits)
		}
	}
	if a.Pending() || len(rev.cuts) != 1 || rev.cuts[0] != "pairwise-1" {
		t.Errorf("pending %v, cuts %v; want the held logout applied and the state published", a.Pending(), rev.cuts)
	}
}
