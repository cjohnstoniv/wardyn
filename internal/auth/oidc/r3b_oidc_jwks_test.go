// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	jose "github.com/go-jose/go-jose/v4"
)

// r3bJWKS serves a JWKS document made of one GOOD ES256 key plus whatever extra
// raw entries the case adds, and returns the URL and a token signed by the good
// key.
func r3bJWKS(t *testing.T, extra ...string) (jwksURL, token string) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate ES256 key: %v", err)
	}
	good, err := json.Marshal(jose.JSONWebKey{
		Key: priv.Public(), KeyID: "good", Algorithm: string(jose.ES256), Use: "sig",
	})
	if err != nil {
		t.Fatalf("marshal good jwk: %v", err)
	}
	keys := append([]string{string(good)}, extra...)
	doc := `{"keys":[`
	for i, k := range keys {
		if i > 0 {
			doc += ","
		}
		doc += k
	}
	doc += `]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(doc))
	}))
	t.Cleanup(srv.Close)

	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: priv, KeyID: "good"}},
		(&jose.SignerOptions{}).WithType("JWT"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	sig, err := signer.Sign([]byte(`{"sub":"x"}`))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	token, err = sig.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return srv.URL, token
}

// TestR3BJWKSSurvivesAnUnrepresentableKey is F242's pin.
//
// On go-oidc v3.20.0 a SINGLE JWKS entry whose `kty` the JOSE stack cannot
// represent — and which carries no `alg` member — made RemoteKeySet fail the
// WHOLE document ("failed to decode keys: ... unsupported key type/format"), so
// no signing key loaded and every ID-token signature check failed. That is
// every SSO login down, caused by one key Wardyn never needed. v3.21.0 skips
// the entry it cannot represent and keeps working on the keys it can.
//
// The `alg`-carrying variant is the case that already worked and is the control
// for the second arm: it is why the bug was invisible on most IdPs.
func TestR3BJWKSSurvivesAnUnrepresentableKey(t *testing.T) {
	ctx := context.Background()

	verify := func(t *testing.T, extra ...string) error {
		t.Helper()
		url, token := r3bJWKS(t, extra...)
		payload, err := gooidc.NewRemoteKeySet(ctx, url).VerifySignature(ctx, token)
		if err == nil && string(payload) != `{"sub":"x"}` {
			t.Fatalf("verified payload = %s, want the signed body", payload)
		}
		return err
	}

	t.Run("A: only supported keys", func(t *testing.T) {
		if err := verify(t); err != nil {
			t.Fatalf("a JWKS of only supported keys failed to verify: %v", err)
		}
	})

	t.Run("B: one unsupported kty with NO alg member", func(t *testing.T) {
		if err := verify(t, `{"kty":"UNSUPPORTED-KTY","kid":"weird","x":"AA"}`); err != nil {
			t.Fatalf("one key this JOSE stack cannot represent failed the WHOLE key set, so no signing key "+
				"loaded and every SSO login would break: %v — go-oidc/v3 must be >= v3.21.0, which skips "+
				"the entry it cannot represent", err)
		}
	})

	t.Run("C: one unsupported kty WITH an alg member", func(t *testing.T) {
		// The shape that worked even on v3.20.0 — go-oidc filtered it out by
		// `alg` before the JOSE stack ever saw it. It is the reason arm B went
		// unnoticed: an IdP that stamps `alg` on every key never hits it.
		if err := verify(t, `{"kty":"UNSUPPORTED-KTY","kid":"weird","alg":"ES256K","x":"AA"}`); err != nil {
			t.Fatalf("an unsupported key carrying an alg broke the key set: %v", err)
		}
	})
}
