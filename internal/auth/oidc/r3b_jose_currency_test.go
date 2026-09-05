// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"strings"
	"testing"

	jose "github.com/go-jose/go-jose/v4"
)

// TestR3BJOSERejectsMalformedEd25519JWK is F241's pin: the floor on
// github.com/go-jose/go-jose/v4, expressed as BEHAVIOUR rather than as a
// version string, so an MVS downgrade or a revert fails here instead of
// silently reopening the window.
//
// v4.1.5 (2026-09-03) shipped seven upstream-labelled security fixes on the two
// paths this package parses tokens Wardyn did not mint — jwt/claims.go,
// json/scanner.go, jws.go, jwk.go, asymmetric.go, cipher/cbc_hmac.go. NO
// ADVISORY WAS FILED for any of them, which is the structural half of the
// finding: govulncheck is advisory-ID driven, so it was green on v4.1.4 and
// would have stayed green forever. Nothing else in CI reads upstream release
// notes, so this test is what holds the floor.
//
// "Reject malformed Ed25519 JWKs" (#250) is the fix pinned here because it is
// reachable through the public API on the exact path that matters: every JWKS
// document the IdP serves is unmarshalled key-by-key through this code before
// an ID-token signature is checked. A key that parses into an Ed25519 public
// key of the wrong length is a key the verifier would then use.
func TestR3BJOSERejectsMalformedEd25519JWK(t *testing.T) {
	for _, tc := range []struct {
		name string
		jwk  string
	}{
		// x decodes to one byte; an Ed25519 public key is exactly 32.
		{"x too short", `{"kty":"OKP","crv":"Ed25519","kid":"bad","x":"AA"}`},
		{"x empty", `{"kty":"OKP","crv":"Ed25519","kid":"bad","x":""}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var k jose.JSONWebKey
			err := k.UnmarshalJSON([]byte(tc.jwk))
			if err == nil {
				t.Fatalf("a malformed Ed25519 JWK (%s) unmarshalled with NO error — go-jose/v4 must be "+
					">= v4.1.5, which rejects it (upstream #250). Nothing else catches this: no advisory "+
					"exists for that release, so govulncheck is green either way", tc.jwk)
			}
			if !strings.Contains(err.Error(), "Ed25519") {
				t.Errorf("rejected with %q, want an error naming the Ed25519 key — a generic failure would "+
					"also come from a version that never looked at the length", err)
			}
		})
	}

	// The control: a WELL-FORMED Ed25519 JWK must still load, so the pin cannot
	// be satisfied by a library (or a future patch) that rejects the whole key
	// type — which would break login on an IdP that signs with EdDSA.
	t.Run("a well-formed Ed25519 JWK still loads", func(t *testing.T) {
		pub, _, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatalf("generate ed25519 key: %v", err)
		}
		jwk := `{"kty":"OKP","crv":"Ed25519","kid":"good","x":"` +
			base64.RawURLEncoding.EncodeToString(pub) + `"}`
		var k jose.JSONWebKey
		if err := k.UnmarshalJSON([]byte(jwk)); err != nil {
			t.Fatalf("a valid Ed25519 JWK was rejected: %v", err)
		}
		if !k.Valid() {
			t.Error("a valid Ed25519 JWK unmarshalled but reports Valid() == false")
		}
	})
}
