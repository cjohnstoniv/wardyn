// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// CLI sign-in is off unless the operator turns it on: every route is a 404 and /healthz says so.
func TestCLISignIn_OffByDefault(t *testing.T) {
	h := newHarness(t)
	for _, srv := range []*Server{h.srv, New(func() Config {
		cfg := baseTestConfig(h, h.srv.cfg.Store)
		cfg.OIDC = &oidc.Authenticator{}
		return cfg
	}())} {
		for _, p := range []struct{ method, path string }{
			{http.MethodPost, "/api/v1/cli/sign-ins"},
			{http.MethodPost, "/api/v1/cli/sign-ins/" + uuid.NewString() + "/token"},
			{http.MethodPost, "/api/v1/cli/sessions/" + uuid.NewString() + "/refresh"},
			{http.MethodGet, "/api/v1/me/cli-sessions"},
		} {
			if w := do(t, srv, p.method, p.path, adminToken, "{}"); w.Code != http.StatusNotFound {
				t.Errorf("%s %s with CLI sign-in off = %d, want 404", p.method, p.path, w.Code)
			}
		}
		if w := do(t, srv, http.MethodGet, "/healthz", "", ""); !strings.Contains(w.Body.String(), `"cli_sign_in":false`) ||
			!strings.Contains(w.Body.String(), `"cli_paste_sign_in":false`) {
			t.Errorf("/healthz with CLI sign-in off = %s, want cli_sign_in and cli_paste_sign_in false", w.Body)
		}
	}
}

// The approved lifetimes: 30 days, 7 days idle, the file store capped at 7 days whatever the TTL, and
// the idle lifetime never above the absolute one.
func TestCLISignIn_ConfigLifetimes(t *testing.T) {
	for _, c := range []struct {
		cfg             CLISignInConfig
		store           string
		wantTTL, wantIn time.Duration
	}{
		{CLISignInConfig{}, types.CLIStoreKeychain, 30 * 24 * time.Hour, 7 * 24 * time.Hour},
		{CLISignInConfig{}, types.CLIStoreFile, 7 * 24 * time.Hour, 7 * 24 * time.Hour},
		{CLISignInConfig{SessionTTL: 90 * 24 * time.Hour}, types.CLIStoreFile, 7 * 24 * time.Hour, 7 * 24 * time.Hour},
		{CLISignInConfig{SessionTTL: 90 * 24 * time.Hour, SessionIdle: 30 * 24 * time.Hour}, types.CLIStoreKeychain, 90 * 24 * time.Hour, 30 * 24 * time.Hour},
		{CLISignInConfig{SessionTTL: 2 * time.Hour}, types.CLIStoreKeychain, 2 * time.Hour, 2 * time.Hour},
		{CLISignInConfig{SessionTTL: 2 * time.Hour}, types.CLIStoreFile, 2 * time.Hour, 2 * time.Hour},
	} {
		if ttl, idle := c.cfg.sessionTTL(c.store), c.cfg.sessionIdle(c.store); ttl != c.wantTTL || idle != c.wantIn {
			t.Errorf("%+v %s = %s / %s idle, want %s / %s", c.cfg, c.store, ttl, idle, c.wantTTL, c.wantIn)
		}
	}
}

func TestCLISignIn_RequestRefusals(t *testing.T) {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	sid := uuid.New()
	good := types.CLISignInCreateRequest{Challenge: base64.RawURLEncoding.EncodeToString(make([]byte, 32)), Port: 4711, Machine: "pm-laptop",
		PublicKey: base64.RawURLEncoding.EncodeToString(pub), Mode: types.CLISignInModeLoopback, Store: types.CLIStoreKeychain}
	if got := cliSignInRequestRefusal(good); got != "" {
		t.Fatalf("a good request is refused: %s", got)
	}
	for field, edit := range map[string]func(*types.CLISignInCreateRequest){
		"challenge": func(r *types.CLISignInCreateRequest) { r.Challenge = "plain-verifier" },
		"public_key": func(r *types.CLISignInCreateRequest) {
			r.PublicKey = base64.RawURLEncoding.EncodeToString(make([]byte, 31))
		},
		"machine":    func(r *types.CLISignInCreateRequest) { r.Machine = strings.Repeat("m", 101) },
		"mode":       func(r *types.CLISignInCreateRequest) { r.Mode = "device" },
		"port":       func(r *types.CLISignInCreateRequest) { r.Port = 0 },
		"store":      func(r *types.CLISignInCreateRequest) { r.Store = "registry" },
		"session_id": func(r *types.CLISignInCreateRequest) { r.SessionID = &sid },
	} {
		req := good
		edit(&req)
		if got := cliSignInRequestRefusal(req); !strings.HasPrefix(got, field+":") {
			t.Errorf("bad %s refused as %q", field, got)
		}
	}
	paste := good
	paste.Mode, paste.Port = types.CLISignInModePaste, 4711
	if got := cliSignInRequestRefusal(paste); !strings.HasPrefix(got, "port:") {
		t.Errorf("a paste sign-in with a port refused as %q, want port", got)
	}
}

// A device proof binds the key, the organisation, the route, the session and the secret, within 5 minutes.
func TestCLISession_ProofBindsEveryField(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	srv := &Server{cfg: Config{CLISignIn: CLISignInConfig{Origin: cliTestOrigin}}}
	id, now := uuid.New(), time.Now()
	const route, secret = "/api/v1/cli/sessions/x/refresh", "wdc_secret"
	sign := func(origin, route string, id uuid.UUID, secret string, at time.Time) types.CLIProof {
		sig := ed25519.Sign(priv, types.CLIProofMessage(origin, route, id, secret, at.Unix()))
		return types.CLIProof{Time: at.Unix(), Signature: base64.RawURLEncoding.EncodeToString(sig)}
	}
	if !srv.cliProofValid(pub, route, id, secret, sign(cliTestOrigin, route, id, secret, now), now) {
		t.Fatal("a good proof is refused")
	}
	for name, p := range map[string]types.CLIProof{
		"another organisation": sign("https://other.test", route, id, secret, now),
		"another route":        sign(cliTestOrigin, "/api/v1/cli/sign-ins/x/token", id, secret, now),
		"another session":      sign(cliTestOrigin, route, uuid.New(), secret, now),
		"another secret":       sign(cliTestOrigin, route, id, "wdc_other", now),
		"301 s old":            sign(cliTestOrigin, route, id, secret, now.Add(-301*time.Second)),
		"301 s ahead":          sign(cliTestOrigin, route, id, secret, now.Add(301*time.Second)),
	} {
		if srv.cliProofValid(pub, route, id, secret, p, now) {
			t.Errorf("a proof for %s passes", name)
		}
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if srv.cliProofValid(other, route, id, secret, sign(cliTestOrigin, route, id, secret, now), now) {
		t.Error("a proof checked against another key passes")
	}
}
