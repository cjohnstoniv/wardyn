// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// jwks_tolerant_test.go pins the property F242's commit CLAIMED and only ever
// half-delivered: one odd entry in the IdP's JWKS cannot break login.
//
// go-oidc v3.21.0 buys that for entries whose `kty`/`crv` it cannot represent
// and nothing else, so a MALFORMED key of a supported type — RFC 7517 section
// 5's other two clauses, "missing required members" and "values out of the
// supported ranges" — still failed the whole document. The go-jose v4.1.5 bump
// (taken for its seven upstream security fixes, and NOT to be reverted) moved
// malformed Ed25519 keys into exactly that unprotected class. These tests hold
// the whole property, so a future MVS change cannot silently move a key shape
// between the protected and unprotected halves again.
package oidc_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	gooidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/coreos/go-oidc/v3/oidc/oidctest"
	jose "github.com/go-jose/go-jose/v4"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// Malformed entries of SUPPORTED key types — the class go-oidc does not skip.
// Each is a well-formed JSON object naming a kty this stack understands, whose
// key material is unusable.
const (
	badEd25519JWK = `{"kty":"OKP","crv":"Ed25519","kid":"bad-okp","x":"AA"}`
	badECJWK      = `{"kty":"EC","crv":"P-256","kid":"bad-ec","x":"AA","y":"AA"}`
	badRSAJWK     = `{"kty":"RSA","kid":"bad-rsa","e":"AQAB"}`

	// goodECJWK is a WELL-FORMED P-256 key, the control every "must pass
	// through untouched" row needs: without it a row could pass merely
	// because nothing survived.
	goodECJWK = `{"kty":"EC","crv":"P-256","kid":"good","x":"f83OJ3D2xF1Bg8vub9tLe1gHMzV76e8Tus9uPHvRVEU","y":"x_FEzRu9m36HLN_tue659LNpXW6pCyStikYjKIWI5a0"}`
)

// TestTolerantJWKSSurvivesAMalformedSupportedKey drives a REAL
// gooidc.RemoteKeySet through the client production uses, against the same
// one-good-ES256-key JWKS helper F242's pin uses.
//
// Counterfactual: swap NewTolerantJWKSClientForTest for a plain &http.Client{}
// (which is what go-oidc gets without this wiring) and every "malformed
// <type>" arm goes red with go-jose's own decode error.
func TestTolerantJWKSSurvivesAMalformedSupportedKey(t *testing.T) {
	ctx := context.Background()

	verify := func(t *testing.T, extra ...string) error {
		t.Helper()
		url, token := r3bJWKS(t, extra...)
		keySetCtx := gooidc.ClientContext(ctx, writoidc.NewTolerantJWKSClientForTest(nil))
		payload, err := gooidc.NewRemoteKeySet(keySetCtx, url).VerifySignature(keySetCtx, token)
		if err == nil && string(payload) != `{"sub":"x"}` {
			t.Fatalf("verified payload = %s, want the signed body", payload)
		}
		return err
	}

	for _, tc := range []struct {
		name  string
		extra string
	}{
		// The regression the go-jose v4.1.5 bump introduced: harmless at
		// v4.1.4 (silently zero-padded), fatal to the whole key set after.
		{"malformed Ed25519 (the v4.1.5 regression)", badEd25519JWK},
		// The two that were broken BEFORE the wave as well — same RFC 7517
		// clause, same outage, never protected by go-oidc's kty filter.
		{"malformed EC", badECJWK},
		{"malformed RSA (missing n)", badRSAJWK},
		// The classes go-oidc already skipped stay working.
		{"unrepresentable kty", `{"kty":"UNSUPPORTED-KTY","kid":"weird","x":"AA"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := verify(t, tc.extra); err != nil {
				t.Fatalf("one unusable JWKS entry (%s) failed the WHOLE key set, so no signing key loaded "+
					"and every SSO login is down: %v", tc.extra, err)
			}
		})
	}

	t.Run("several bad entries at once", func(t *testing.T) {
		if err := verify(t, badEd25519JWK, badECJWK, badRSAJWK,
			`{"kty":"UNSUPPORTED-KTY","kid":"weird","x":"AA"}`); err != nil {
			t.Fatalf("a key set with four unusable entries beside one good key failed to verify: %v", err)
		}
	})

	t.Run("only good keys is untouched", func(t *testing.T) {
		if err := verify(t); err != nil {
			t.Fatalf("a JWKS of only supported keys failed to verify: %v", err)
		}
	})

	// The floor this must NOT cross: tolerance is "the good keys still work",
	// never "an unusable key verifies something". A token signed by a key that
	// is not in the set at all is still refused.
	t.Run("a foreign signature is still refused", func(t *testing.T) {
		url, _ := r3bJWKS(t, badEd25519JWK)
		_, foreign := r3bJWKS(t) // signed by a DIFFERENT good key
		keySetCtx := gooidc.ClientContext(ctx, writoidc.NewTolerantJWKSClientForTest(nil))
		if _, err := gooidc.NewRemoteKeySet(keySetCtx, url).VerifySignature(keySetCtx, foreign); err == nil {
			t.Fatal("a token signed by a key absent from the JWKS verified: the tolerant fetch must only ever " +
				"REMOVE unusable entries, never widen what counts as a valid signature")
		}
	})

	// When nothing survives, the operator must keep the error that names the
	// broken key rather than a generic "no key matched" — login is equally
	// down either way, so the diagnostic is the whole difference.
	t.Run("all keys malformed keeps the precise error", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"keys":[` + badECJWK + `,` + badRSAJWK + `]}`))
		}))
		t.Cleanup(srv.Close)
		_, token := r3bJWKS(t)
		keySetCtx := gooidc.ClientContext(ctx, writoidc.NewTolerantJWKSClientForTest(nil))
		_, err := gooidc.NewRemoteKeySet(keySetCtx, srv.URL).VerifySignature(keySetCtx, token)
		if err == nil {
			t.Fatal("a key set of nothing but malformed keys verified a token")
		}
		if !strings.Contains(err.Error(), "EC public key") {
			t.Errorf("error = %v, want go-jose's own complaint naming the malformed key — replacing it with a "+
				"generic failure hides which key the operator has to go fix", err)
		}
	})
}

// TestFilterJWKSPassesThroughWhatItDoesNotUnderstand pins the rewrite's
// conservative half. Every shape here must come back BYTE-IDENTICAL, so a
// response this layer is not sure about reaches go-oidc exactly as it would
// have without this file, and go-oidc's own error is what an operator sees.
func TestFilterJWKSPassesThroughWhatItDoesNotUnderstand(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"not JSON at all", `<html>nope</html>`},
		{"JSON, but not an object", `["keys"]`},
		{"an object with no keys member", `{"issuer":"https://idp.example"}`},
		{"keys is not an array", `{"keys":"nope"}`},
		{"every key is fine", `{"keys":[` + goodECJWK + `]}`},
		{"nothing survives", `{"keys":[` + badECJWK + `]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, dropped := writoidc.FilterJWKSForTest([]byte(tc.body))
			if string(out) != tc.body {
				t.Errorf("body was rewritten to %s, want it passed through untouched", out)
			}
			if dropped != 0 {
				t.Errorf("dropped = %d, want 0 — a pass-through must not report drops", dropped)
			}
		})
	}

	t.Run("a mixed set keeps the good entries verbatim and reports the drop", func(t *testing.T) {
		out, dropped := writoidc.FilterJWKSForTest([]byte(`{"keys":[` + goodECJWK + `,` + badRSAJWK + `]}`))
		if dropped != 1 {
			t.Fatalf("dropped = %d, want 1", dropped)
		}
		if !strings.Contains(string(out), `"kid":"good"`) {
			t.Errorf("out = %s, want the surviving key carried through", out)
		}
		if strings.Contains(string(out), "bad-rsa") {
			t.Errorf("out = %s, want the malformed key removed", out)
		}
	})
}

// TestTolerantJWKSEndToEndLogin is the wiring pin, and it is the one F325's
// finding is really about: a behavioural floor that talks to go-oidc DIRECTLY
// stays green while Wardyn's own login is down, because nothing proves the
// Authenticator actually fetches its key set through the tolerant client.
//
// So this drives the real thing: writoidc.New against an IdP whose published
// JWKS carries one malformed Ed25519 entry beside the good RS256 key, then a
// real /auth/callback. It fails if anyone rewires New back to
// provider.Verifier.
func TestTolerantJWKSEndToEndLogin(t *testing.T) {
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	goodJWK, err := json.Marshal(jose.JSONWebKey{
		Key: priv.Public(), KeyID: "good", Algorithm: "RS256", Use: "sig",
	})
	if err != nil {
		t.Fatalf("marshal good jwk: %v", err)
	}

	var issuer, idToken string
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"issuer":%q,"authorization_endpoint":%q,"token_endpoint":%q,"jwks_uri":%q,`+
			`"id_token_signing_alg_values_supported":["RS256"],"response_types_supported":["code"],`+
			`"subject_types_supported":["public"]}`,
			issuer, issuer+"/auth", issuer+"/token", issuer+"/keys")
	})
	// The poisoned key set: one good RS256 key, one malformed Ed25519 entry.
	mux.HandleFunc("/keys", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"keys":[` + string(goodJWK) + `,` + badEd25519JWK + `]}`))
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "at", "token_type": "Bearer", "expires_in": 3600, "id_token": idToken,
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	issuer = srv.URL

	claims, err := json.Marshal(map[string]any{
		"iss": issuer, "sub": "sub-jwks", "aud": "wardyn-client",
		"email": "alice@corp.example", "email_verified": true, "nonce": roleCallbackNonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	idToken = oidctest.SignIDToken(priv, "good", "RS256", string(claims))

	auth, err := writoidc.New(context.Background(), writoidc.Config{
		IssuerURL:    issuer,
		ClientID:     "wardyn-client",
		ClientSecret: "secret",
		RedirectURL:  "http://localhost/auth/callback",
	}, testHMACKey)
	if err != nil {
		t.Fatalf("writoidc.New against an IdP with one malformed JWKS entry: %v", err)
	}

	w, sess := doCallback(t, auth)
	if sess.Sub != "sub-jwks" {
		t.Fatalf("login failed (status %d, %q, body %q): one malformed Ed25519 entry in the IdP's JWKS "+
			"took down the whole key set, so no id_token signature could be verified and every human is "+
			"locked out of the console",
			w.Code, w.Result().Header.Get("Location"), w.Body.String())
	}
}
