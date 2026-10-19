// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gooidctest "github.com/coreos/go-oidc/v3/oidc/oidctest"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// backchannelServer builds a Server whose identity provider counts the requests made after discovery
// (its key-set fetches), and a signer for logout tokens under the provider's key or another one.
func backchannelServer(t *testing.T, on bool) (*Server, *atomic.Int64, func(key *rsa.PrivateKey, kid string) string, *rsa.PrivateKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	idp := &gooidctest.Server{PublicKeys: []gooidctest.PublicKey{{PublicKey: priv.Public(), KeyID: "bc-key", Algorithm: "RS256"}}}
	var fetches atomic.Int64
	httpSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, "openid-configuration") {
			fetches.Add(1)
		}
		idp.ServeHTTP(w, r)
	}))
	idp.SetIssuer(httpSrv.URL)
	t.Cleanup(httpSrv.Close)
	auth, err := oidc.New(context.Background(), oidc.Config{
		IssuerURL: httpSrv.URL, ClientID: "wardyn-client", ClientSecret: "secret", RedirectURL: "http://localhost/auth/callback",
		Revocations: fakeAuthzSessionRevocations{}, BackchannelLogout: on,
	}, accessTestHMACKey)
	if err != nil {
		t.Fatalf("oidc.New: %v", err)
	}
	fixed := time.Now()
	sign := func(key *rsa.PrivateKey, kid string) string {
		raw, _ := json.Marshal(map[string]any{"iss": httpSrv.URL, "aud": "wardyn-client", "sub": "pairwise-1", "jti": "j",
			"iat": time.Now().Unix(), "exp": time.Now().Add(time.Minute).Unix(),
			"events": map[string]any{"http://schemas.openid.net/event/backchannel-logout": map[string]any{}}})
		return gooidctest.SignIDToken(key, kid, "RS256", string(raw))
	}
	return New(Config{OIDC: auth, Now: func() time.Time { return fixed }}), &fetches, sign, priv
}

func postBackchannel(t *testing.T, srv *Server, token string) int {
	r := httptest.NewRequest(http.MethodPost, "/auth/backchannel-logout", strings.NewReader(url.Values{"logout_token": {token}}.Encode()))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	w := httptest.NewRecorder()
	panicFails(t, srv.Handler()).ServeHTTP(w, r)
	return w.Code
}

// The logout route exists only when the operator turned it on.
func TestBackchannelLogoutRouteMountedOnlyWhenOn(t *testing.T) {
	off, _, _, _ := backchannelServer(t, false)
	if code := postBackchannel(t, off, ""); code != http.StatusNotFound && code != http.StatusMethodNotAllowed {
		t.Errorf("flag off: POST /auth/backchannel-logout = %d, want 404 or 405", code)
	}
	on, _, _, _ := backchannelServer(t, true)
	if code := postBackchannel(t, on, ""); code != http.StatusBadRequest {
		t.Errorf("flag on: POST with no token = %d, want the handler's 400", code)
	}
}

// Forged logout tokens, signed by a key the provider never published, each make the verifier refetch
// the provider's keys; past backchannelLogoutBurst they are refused before verification. Tokens the
// provider signed give their charge back, so a run of them is never refused.
func TestBackchannelLogoutBoundsKeyFetchesFromForgedTokens(t *testing.T) {
	srv, fetches, sign, priv := backchannelServer(t, true)
	for i := range 3 * int(backchannelLogoutBurst) {
		if code := postBackchannel(t, srv, sign(priv, "bc-key")); code != http.StatusOK {
			t.Fatalf("genuine logout %d = %d, want 200", i, code)
		}
	}
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	forged := sign(other, "unknown-kid")
	before, refused := fetches.Load(), 0
	for range 20 {
		if postBackchannel(t, srv, forged) == http.StatusTooManyRequests {
			refused++
		}
	}
	if got := fetches.Load() - before; got > int64(backchannelLogoutBurst) || refused == 0 {
		t.Errorf("20 forged POSTs made %d key fetches with %d refused, want at most %v fetches", got, refused, backchannelLogoutBurst)
	}
}
