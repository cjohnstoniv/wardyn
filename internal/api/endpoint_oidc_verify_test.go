// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// oidcIssuer is a bare authority: a discovery document and a key set, and a signer for any claims a case needs.
type oidcIssuer struct {
	srv *httptest.Server
	key *rsa.PrivateKey
	kid string
	url string
}

func newOIDCIssuer(t *testing.T) *oidcIssuer {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	is := &oidcIssuer{key: key, kid: "key-1"}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer": is.url, "authorization_endpoint": is.url + "/authorize", "token_endpoint": is.url + "/token",
			"jwks_uri": is.url + "/keys", "response_types_supported": []string{"code"},
			"subject_types_supported": []string{"public"}, "id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{
			{Key: &key.PublicKey, KeyID: is.kid, Algorithm: "RS256", Use: "sig"},
		}})
	})
	is.srv = httptest.NewServer(mux)
	is.url = is.srv.URL
	t.Cleanup(is.srv.Close)
	return is
}

func (is *oidcIssuer) claims(edit func(map[string]any)) map[string]any {
	c := map[string]any{
		"iss": is.url, "aud": "client-1", "sub": "subject-1", "nonce": "nonce-1",
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	}
	if edit != nil {
		edit(c)
	}
	return c
}

// sign signs claims with key (the issuer's, or another) under kid; "" omits the key id from the header.
func (is *oidcIssuer) sign(t *testing.T, key *rsa.PrivateKey, kid string, alg jose.SignatureAlgorithm, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: alg, Key: jose.JSONWebKey{Key: key, KeyID: kid}},
		(&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(claims)
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	out, err := sig.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func rawJWT(header map[string]any, claims map[string]any, sig []byte) string {
	h, _ := json.Marshal(header)
	c, _ := json.Marshal(claims)
	enc := base64.RawURLEncoding.EncodeToString
	return enc(h) + "." + enc(c) + "." + enc(sig)
}

func TestEndpointOIDC_IdentityTokenVerification(t *testing.T) {
	is := newOIDCIssuer(t)
	other, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := gooidc.NewProvider(context.Background(), is.url)
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	b := &endpointOIDCBinding{
		row:  types.ModelProvider{UID: oidcUID, OIDC: &types.ProviderOIDC{ClientID: "client-1"}},
		disc: endpointOIDCDiscovery{provider: provider},
	}
	s := &Server{}
	good := is.sign(t, is.key, is.kid, jose.RS256, is.claims(nil))
	hmacTok := func() string {
		head := map[string]any{"alg": "HS256", "kid": is.kid, "typ": "JWT"}
		mac := hmac.New(sha256.New, []byte("a shared secret an attacker chose"))
		mac.Write([]byte("signed"))
		return rawJWT(head, is.claims(nil), mac.Sum(nil))
	}()

	for _, tc := range []struct {
		name  string
		token string
		nonce string
		want  bool // accepted
	}{
		{"a good token", good, "nonce-1", true},
		{"a bad signature", is.sign(t, other, is.kid, jose.RS256, is.claims(nil)), "nonce-1", false},
		{"a wrong issuer", is.sign(t, is.key, is.kid, jose.RS256, is.claims(func(c map[string]any) { c["iss"] = "https://elsewhere.example" })), "nonce-1", false},
		{"a wrong audience", is.sign(t, is.key, is.kid, jose.RS256, is.claims(func(c map[string]any) { c["aud"] = "client-2" })), "nonce-1", false},
		{"an expired token", is.sign(t, is.key, is.kid, jose.RS256, is.claims(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Hour).Unix() })), "nonce-1", false},
		{"a nonce that is not this attempt's", good, "nonce-2", false},
		{"no nonce in the token", is.sign(t, is.key, is.kid, jose.RS256, is.claims(func(c map[string]any) { delete(c, "nonce") })), "nonce-1", false},
		{"no nonce expected", good, "", false},
		{"alg none", rawJWT(map[string]any{"alg": "none", "kid": is.kid, "typ": "JWT"}, is.claims(nil), nil), "nonce-1", false},
		{"a symmetric algorithm", hmacTok, "nonce-1", false},
		{"an algorithm outside the accepted set", is.sign(t, is.key, is.kid, jose.RS384, is.claims(nil)), "nonce-1", false},
		{"a missing kid", is.sign(t, is.key, "", jose.RS256, is.claims(nil)), "nonce-1", false},
		{"a token that is not three parts", "not.a-token", "nonce-1", false},
		{"no token", "", "nonce-1", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			idToken, reason := s.verifyEndpointOIDCIDToken(context.Background(), b, tc.token, tc.nonce)
			if tc.want {
				if reason != "" || idToken == nil || idToken.Subject != "subject-1" {
					t.Fatalf("refused a good token: %q", reason)
				}
				return
			}
			if reason == "" || idToken != nil {
				t.Fatalf("accepted %s", tc.name)
			}
			if tc.token != "" && strings.Contains(reason, tc.token) {
				t.Errorf("the refusal carries the token: %q", reason)
			}
		})
	}
}
