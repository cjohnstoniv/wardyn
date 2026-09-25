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

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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

// TestJWKSSurvivesAnUnrepresentableKey pins exactly the dependency floor —
// not the broader property.
//
// Below go-oidc v3.21.0, a single JWKS entry whose `kty` the JOSE stack
// cannot represent — and which carries no `alg` member — makes RemoteKeySet
// fail the whole document ("failed to decode keys: ... unsupported key
// type/format"), so no signing key loads and every ID-token signature check
// fails: every SSO login down, caused by one key Wardyn never needed. v3.21.0
// skips the entry it cannot represent and keeps working on the keys it can.
//
// What that does and does not buy. go-oidc v3.21.0 quotes RFC 7517 section 5
// — "Implementations SHOULD ignore JWKs within a JWK Set that use `kty`
// values that are not understood by them, that are missing required members,
// or for which values are out of the supported ranges" — and implements the
// first clause only. An unrepresentable key type is skipped; a malformed key
// of a supported type (a short EC `x`, an RSA missing `n`, an Ed25519 `x`
// that is not 32 bytes) still fails the whole document. Arms that exercise
// only the narrow property cannot vouch for the broad one.
//
// So arms A-C below are the dependency floor and use go-oidc RAW: they answer
// "is go-oidc still at least v3.21.0", and an MVS downgrade turns arm B red.
// Arm D is the broad property, and it goes through the key set Wardyn
// actually builds, because that is where the broad property comes from —
// tolerantJWKSTransport, not the dependency. Its depth (EC, RSA, several at
// once, and the floor that an unusable key still verifies nothing) lives in
// jwks_tolerant_test.go.
//
// The `alg`-carrying variant is the control for the second arm: it is why the
// failure is invisible on most IdPs.
func TestJWKSSurvivesAnUnrepresentableKey(t *testing.T) {
	// ticket: R3B
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

	// Arm D: the broad property, on the key set Wardyn builds. A malformed key
	// of a supported type is the class arms A-C never touch and go-oidc does
	// not skip — the class malformed Ed25519 (under go-jose v4.1.5), EC and RSA
	// entries are all in.
	//
	// It uses NewTolerantJWKSClientForTest deliberately: swap it for the plain
	// context arms A-C use and this goes red, which is the whole point — the
	// tolerance is Wardyn's (tolerantJWKSTransport), not the dependency's, and
	// a future MVS change that moves a key shape between the two classes cannot
	// pass unnoticed in either direction.
	t.Run("D: one MALFORMED key of a supported type, through Wardyn's key set", func(t *testing.T) {
		url, token := r3bJWKS(t, `{"kty":"OKP","crv":"Ed25519","kid":"bad","x":"AA"}`)
		keySetCtx := gooidc.ClientContext(ctx, writoidc.NewTolerantJWKSClientForTest(nil))
		payload, err := gooidc.NewRemoteKeySet(keySetCtx, url).VerifySignature(keySetCtx, token)
		if err != nil {
			t.Fatalf("one malformed Ed25519 entry failed the WHOLE key set, so no signing key loaded and every "+
				"SSO login is down: %v — go-oidc skips only an unrepresentable key TYPE (RFC 7517 s5's first "+
				"clause); the other two clauses are tolerantJWKSTransport's job", err)
		}
		if string(payload) != `{"sub":"x"}` {
			t.Fatalf("verified payload = %s, want the signed body", payload)
		}
	})

	// Arm E: the same clause, on the two shapes no dependency version handles.
	// A malformed EC or RSA entry is accepted by no go-jose version on any
	// go-oidc — it fails the whole key set — so "one odd JWKS key cannot break
	// login" holds for this class only because the tolerance is Wardyn's own
	// layer rather than a pin. That is why this arm is here, in the file that
	// makes the claim, rather than only in jwks_tolerant_test.go where the
	// rest of the depth (several bad entries at once, and the floor that an
	// unusable key still verifies nothing) lives.
	// Both shapes in ONE key set: a real IdP publishing junk rarely publishes
	// exactly one piece of it, and the good key must survive all of it.
	t.Run("E: malformed EC and RSA entries, the class that predates the wave", func(t *testing.T) {
		url, token := r3bJWKS(t,
			`{"kty":"EC","crv":"P-256","kid":"bad-ec","x":"AA","y":"AA"}`,
			`{"kty":"RSA","kid":"bad-rsa","e":"AQAB"}`)
		keySetCtx := gooidc.ClientContext(ctx, writoidc.NewTolerantJWKSClientForTest(nil))
		payload, err := gooidc.NewRemoteKeySet(keySetCtx, url).VerifySignature(keySetCtx, token)
		if err != nil {
			t.Fatalf("a malformed EC or RSA entry failed the WHOLE key set, so no signing key loaded and every "+
				"SSO login is down: %v — this class is not a regression and no go-jose/go-oidc version fixes "+
				"it; RFC 7517 s5's \"missing required members\" and \"values out of the supported ranges\" "+
				"clauses are tolerantJWKSTransport's to honour", err)
		}
		if string(payload) != `{"sub":"x"}` {
			t.Fatalf("verified payload = %s, want the signed body", payload)
		}
	})
}
