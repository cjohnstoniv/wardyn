// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// credentialedProxy stands in for a corporate proxy that demands Basic
// Proxy-Authorization and answers the OIDC discovery document for issuer. It
// counts the discovery requests that arrived authenticated.
func credentialedProxy(t *testing.T, issuer, user, pass string) (*httptest.Server, *int) {
	t.Helper()
	authed := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := parseProxyBasic(r.Header.Get("Proxy-Authorization"))
		if !ok || u != user || p != pass {
			w.Header().Set("Proxy-Authenticate", `Basic realm="corp"`)
			w.WriteHeader(http.StatusProxyAuthRequired)
			return
		}
		if r.RequestURI != issuer+"/.well-known/openid-configuration" {
			http.NotFound(w, r)
			return
		}
		authed++
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                issuer,
			"authorization_endpoint":                issuer + "/auth",
			"token_endpoint":                        issuer + "/token",
			"jwks_uri":                              issuer + "/keys",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	}))
	t.Cleanup(srv.Close)
	return srv, &authed
}

func parseProxyBasic(h string) (user, pass string, ok bool) {
	req := &http.Request{Header: http.Header{"Authorization": {h}}}
	return req.BasicAuth()
}

func oidcDiscover(tr *http.Transport, issuer string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, &http.Client{Transport: tr})
	_, err := oidc.New(ctx, oidc.Config{IssuerURL: issuer, ClientID: "wardyn", RedirectURL: "https://wardyn.example/auth/callback"}, []byte(strings.Repeat("k", 32)))
	return err
}

// TestInstallDaemonProxySecret_OIDCDiscoveryCarriesTheProxyCredential is the
// hermetic form of the credentialed-proxy walk for WARDYN_DAEMON_PROXY_SECRET
// (#719): the daemon's OIDC discovery goes through the proxy named in the
// secret file and arrives carrying that file's credential, and the same
// discovery through the proxy URL without the credential is refused by it.
func TestInstallDaemonProxySecret_OIDCDiscoveryCarriesTheProxyCredential(t *testing.T) {
	const issuer = "http://idp.corp.invalid"
	proxy, authed := credentialedProxy(t, issuer, "alice", "s3cr3t-token")

	withSecret := freshTransport()
	path := writeProxySecretFile(t, "http://alice:s3cr3t-token@"+strings.TrimPrefix(proxy.URL, "http://")+"\n", 0o600)
	if _, err := installDaemonProxySecret(withSecret, path, ""); err != nil {
		t.Fatalf("installDaemonProxySecret: %v", err)
	}
	if err := oidcDiscover(withSecret, issuer); err != nil {
		t.Fatalf("OIDC discovery through the credentialed proxy: %v", err)
	}
	if *authed != 1 {
		t.Fatalf("authenticated discovery requests at the proxy = %d, want 1", *authed)
	}

	plain := freshTransport()
	if _, err := installDaemonProxy(plain, proxy.URL, ""); err != nil {
		t.Fatalf("installDaemonProxy: %v", err)
	}
	err := oidcDiscover(plain, issuer)
	if err == nil || !strings.Contains(err.Error(), "407") {
		t.Fatalf("discovery through the same proxy without the credential = %v, want a 407 refusal", err)
	}
}
