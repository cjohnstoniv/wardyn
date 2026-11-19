// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gatewayidentity

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/identity/embedded"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

var testRotatedAt = time.Date(2025, 11, 2, 12, 0, 0, 0, time.UTC)

func testClaims() Claims {
	return Claims{
		Issuer:   "spiffe://wardyn.local",
		Audience: "gw-audience",
		Subject:  "person@example.com",
		Run:      "11111111-1111-1111-1111-111111111111",
		Runner:   "docker",
		Provider: "corp-gateway",
	}
}

func testSigner(t *testing.T, raw []byte, now time.Time) *Signer {
	t.Helper()
	s, err := New(raw)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	s.now = func() time.Time { return now }
	return s
}

func testRotate(t *testing.T, raw []byte, drop bool) []byte {
	t.Helper()
	next, err := Rotate(raw, drop, testRotatedAt)
	if err != nil {
		t.Fatalf("Rotate: %v", err)
	}
	return next
}

// testKeySet fetches the published set as a gateway does: the JSON bytes, parsed.
func testKeySet(t *testing.T, s *Signer) jose.JSONWebKeySet {
	t.Helper()
	body, err := s.JWKS()
	if err != nil {
		t.Fatalf("JWKS: %v", err)
	}
	var set jose.JSONWebKeySet
	if err := json.Unmarshal(body, &set); err != nil {
		t.Fatalf("parse JWKS %s: %v", body, err)
	}
	return set
}

func testKids(set jose.JSONWebKeySet) []string {
	kids := make([]string, 0, len(set.Keys))
	for _, k := range set.Keys {
		kids = append(kids, k.KeyID)
	}
	return kids
}

// testVerify checks token against the set by its kid and ES256 alone, the way a
// gateway does, and returns the claims.
func testVerify(t *testing.T, token string, set jose.JSONWebKeySet) (Claims, error) {
	t.Helper()
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("parse token: %v", err)
	}
	var got Claims
	err = parsed.Claims(set, &got)
	return got, err
}

// The token's exact key set is what the doc promises and what a gateway rule
// reads: a claim added or dropped goes red here.
func TestSignExactClaimKeys(t *testing.T) {
	want := []string{"act", "aud", "exp", "iat", "iss", "jti", "sub", "wardyn_format", "wardyn_provider", "wardyn_run", "wardyn_runner"}
	for name, tt := range map[string]struct {
		sponsor string
		want    []string
	}{
		"person run":  {"", want},
		"service run": {"owner@example.com", append(slices.Clone(want), "sponsor")},
	} {
		t.Run(name, func(t *testing.T) {
			now := testRotatedAt
			s := testSigner(t, testRotate(t, nil, false), now)
			c := testClaims()
			c.Sponsor = tt.sponsor
			token, signed, err := s.Sign(c, 0)
			if err != nil {
				t.Fatalf("Sign: %v", err)
			}
			parts := strings.Split(token, ".")
			if len(parts) != 3 {
				t.Fatalf("token has %d parts", len(parts))
			}
			body, err := base64.RawURLEncoding.DecodeString(parts[1])
			if err != nil {
				t.Fatal(err)
			}
			var m map[string]any
			if err := json.Unmarshal(body, &m); err != nil {
				t.Fatal(err)
			}
			keys := make([]string, 0, len(m))
			for k := range m {
				keys = append(keys, k)
			}
			slices.Sort(keys)
			wantKeys := slices.Clone(tt.want)
			slices.Sort(wantKeys)
			if !slices.Equal(keys, wantKeys) {
				t.Errorf("claim keys = %v, want %v", keys, wantKeys)
			}
			if m["aud"] != "gw-audience" || m["sub"] != "person@example.com" || m["wardyn_format"] != "1" {
				t.Errorf("claims = %v", m)
			}
			if got := m["act"].(map[string]any)["sub"]; got != "spiffe://wardyn.local/agent-run/"+c.Run {
				t.Errorf("act.sub = %v", got)
			}
			if signed.ExpiresAt-signed.IssuedAt != int64((10*time.Minute)/time.Second) {
				t.Errorf("iat/exp = %d/%d, want a ten-minute life", signed.IssuedAt, signed.ExpiresAt)
			}
			if _, err := uuid.Parse(signed.ID); err != nil {
				t.Errorf("jti %q: %v", signed.ID, err)
			}
		})
	}
}

func TestSignRefusesAMissingIdentityField(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	for name, clear := range map[string]func(*Claims){
		"issuer":   func(c *Claims) { c.Issuer = "" },
		"audience": func(c *Claims) { c.Audience = "" },
		"subject":  func(c *Claims) { c.Subject = "" },
		"run":      func(c *Claims) { c.Run = "" },
		"provider": func(c *Claims) { c.Provider = "" },
	} {
		c := testClaims()
		clear(&c)
		if token, _, err := s.Sign(c, 0); err == nil {
			t.Errorf("%s empty: signed %q", name, token)
		}
	}
}

// An MCP server's token names the server and no provider; a token naming both, or neither, is refused.
func TestSignMCPToken(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), testRotatedAt)
	c := testClaims()
	c.Provider, c.MCP = "", "corp-tools"
	token, _, err := s.Sign(c, 0)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	body, err := base64.RawURLEncoding.DecodeString(strings.Split(token, ".")[1])
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatal(err)
	}
	if _, has := m["wardyn_provider"]; has || m["wardyn_mcp"] != "corp-tools" {
		t.Errorf("claims = %v, want wardyn_mcp and no wardyn_provider", m)
	}
	for name, mutate := range map[string]func(*Claims){
		"both":    func(c *Claims) { c.Provider = "corp-gateway" },
		"neither": func(c *Claims) { c.MCP = "" },
	} {
		bad := c
		mutate(&bad)
		if token, _, err := s.Sign(bad, 0); err == nil {
			t.Errorf("%s: signed %q", name, token)
		}
	}
}

// The caller's lifetime sets exp; a lifetime that is zero, negative or past the
// cap gives the cap.
func TestSignLifetime(t *testing.T) {
	now := testRotatedAt
	s := testSigner(t, testRotate(t, nil, false), now)
	for name, tt := range map[string]struct{ ask, want time.Duration }{
		"a minute":     {time.Minute, time.Minute},
		"five minutes": {5 * time.Minute, 5 * time.Minute},
		"the cap":      {tokenLifetime, tokenLifetime},
		"unset":        {0, tokenLifetime},
		"negative":     {-time.Minute, tokenLifetime},
		"past the cap": {time.Hour, tokenLifetime},
	} {
		_, signed, err := s.Sign(testClaims(), tt.ask)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := time.Duration(signed.ExpiresAt-signed.IssuedAt) * time.Second; got != tt.want {
			t.Errorf("%s: life = %s, want %s", name, got, tt.want)
		}
	}
}

// ES256, the kid in the header, and a real verify against the published set.
func TestSignVerifiesAgainstThePublishedKeySet(t *testing.T) {
	s := testSigner(t, testRotate(t, nil, false), time.Now())
	token, _, err := s.Sign(testClaims(), 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatalf("not ES256: %v", err)
	}
	if h := parsed.Headers[0]; h.KeyID != s.KeyID() || h.Algorithm != "ES256" {
		t.Errorf("header kid=%q alg=%q, want kid=%q ES256", h.KeyID, h.Algorithm, s.KeyID())
	}
	got, err := testVerify(t, token, testKeySet(t, s))
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if got.Run != testClaims().Run {
		t.Errorf("verified claims = %+v", got)
	}
	body, _ := s.JWKS()
	if strings.Contains(string(body), `"d"`) {
		t.Errorf("JWKS carries a private component: %s", body)
	}
	for _, k := range testKeySet(t, s).Keys {
		if !k.IsPublic() || k.Algorithm != "ES256" || k.Use != "sig" {
			t.Errorf("published key: public=%v alg=%q use=%q", k.IsPublic(), k.Algorithm, k.Use)
		}
	}
}

// A gateway's own verifier, not a reading of the fields: the token is good at its
// audience for its lifetime, and refused past exp, at another audience and from
// another issuer. It has no nbf, so a gateway clock a little behind wardynd's does
// not refuse a token that has just been issued.
func TestAGatewayVerifierEnforcesExpiryAndAudience(t *testing.T) {
	issued := time.Now().UTC().Truncate(time.Second)
	s := testSigner(t, testRotate(t, nil, false), issued)
	token, signed, err := s.Sign(testClaims(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if got := time.Unix(signed.ExpiresAt, 0).UTC(); !got.Equal(issued.Add(tokenLifetime)) {
		t.Fatalf("exp = %s", got)
	}
	parsed, err := jwt.ParseSigned(token, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	var std jwt.Claims
	if err := parsed.Claims(testKeySet(t, s), &std); err != nil {
		t.Fatalf("verify: %v", err)
	}
	if std.NotBefore != nil {
		t.Errorf("nbf = %v, want none", std.NotBefore)
	}
	want := jwt.Expected{Issuer: "spiffe://wardyn.local", AnyAudience: jwt.Audience{"gw-audience"}}
	at := func(when time.Time, e jwt.Expected) error {
		e.Time = when
		return std.ValidateWithLeeway(e, 0)
	}
	if err := at(issued, want); err != nil {
		t.Errorf("at issue: %v", err)
	}
	if err := at(issued.Add(tokenLifetime-time.Second), want); err != nil {
		t.Errorf("just inside its lifetime: %v", err)
	}
	if err := at(issued.Add(tokenLifetime+time.Second), want); !errors.Is(err, jwt.ErrExpired) {
		t.Errorf("past exp: %v, want ErrExpired", err)
	}
	other := want
	other.AnyAudience = jwt.Audience{"another-gateway"}
	if err := at(issued, other); !errors.Is(err, jwt.ErrInvalidAudience) {
		t.Errorf("another audience: %v, want ErrInvalidAudience", err)
	}
	wrong := want
	wrong.Issuer = "wardyn.local"
	if err := at(issued, wrong); !errors.Is(err, jwt.ErrInvalidIssuer) {
		t.Errorf("the bare trust domain as issuer: %v, want ErrInvalidIssuer", err)
	}
}

type testNoAudit struct{}

func (testNoAudit) Record(context.Context, types.AuditEvent) error { return nil }

type testNoRevocations struct{}

func (testNoRevocations) IsRevoked(context.Context, string, uuid.UUID) (bool, error) {
	return false, nil
}
func (testNoRevocations) RevokeRun(context.Context, uuid.UUID) error         { return nil }
func (testNoRevocations) RevokeJTI(context.Context, string, uuid.UUID) error { return nil }

// A gateway token can never authenticate inward: the run token's verifier
// refuses it, whichever audience it is asked for and even with a typed audience
// of the internal one.
func TestRunTokenVerifierRefusesAGatewayToken(t *testing.T) {
	runKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	provider, err := embedded.New(runKey, "", testNoRevocations{}, testNoAudit{})
	if err != nil {
		t.Fatal(err)
	}
	s := testSigner(t, testRotate(t, nil, false), time.Now())
	for _, aud := range []string{"gw-audience", "wardyn-internal"} {
		c := testClaims()
		c.Audience = aud
		token, _, err := s.Sign(c, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := provider.Verify(context.Background(), token, aud); err == nil {
			t.Errorf("audience %q: the run-token verifier accepted a gateway token", aud)
		}
	}
}

// A planned rotation: signing moves at once, both keys are served, a token from
// the old key still verifies, and the row holds the old key's public half only.
func TestRotateServesBothThenRetiresThePrevious(t *testing.T) {
	first := testRotate(t, nil, false)
	old := testSigner(t, first, testRotatedAt)
	oldToken, _, err := old.Sign(testClaims(), 0)
	if err != nil {
		t.Fatal(err)
	}
	second := testRotate(t, first, false)

	var stored storedKeys
	if err := json.Unmarshal(second, &stored); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stored.Previous.Public, "PRIVATE") {
		t.Errorf("the row keeps the previous key's private half: %s", stored.Previous.Public)
	}
	if want := testRotatedAt.Add(15 * time.Minute); !stored.Previous.RetireAt.Equal(want) {
		t.Errorf("retire at %v, want %v (token life plus overlap)", stored.Previous.RetireAt, want)
	}

	s := testSigner(t, second, testRotatedAt)
	if s.KeyID() == old.KeyID() {
		t.Fatal("rotation kept the key")
	}
	if got := testKids(testKeySet(t, s)); !slices.Equal(got, []string{s.KeyID(), old.KeyID()}) {
		t.Errorf("JWKS kids = %v, want new then old", got)
	}
	newToken, _, err := s.Sign(testClaims(), 0)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := jwt.ParseSigned(newToken, []jose.SignatureAlgorithm{jose.ES256})
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Headers[0].KeyID != s.KeyID() {
		t.Errorf("signed with kid %q, want the new key %q", parsed.Headers[0].KeyID, s.KeyID())
	}
	if _, err := testVerify(t, oldToken, testKeySet(t, s)); err != nil {
		t.Errorf("a token from the previous key does not verify inside the overlap: %v", err)
	}

	// One signer, no restart: the same object stops publishing at the retire time.
	now := testRotatedAt.Add(15*time.Minute - time.Second)
	s.now = func() time.Time { return now }
	if got := testKids(testKeySet(t, s)); len(got) != 2 {
		t.Errorf("a second before retire: kids = %v, want both", got)
	}
	now = testRotatedAt.Add(15 * time.Minute)
	if got := testKids(testKeySet(t, s)); !slices.Equal(got, []string{s.KeyID()}) {
		t.Errorf("at retire: kids = %v, want the new key alone", got)
	}
	if _, err := testVerify(t, oldToken, testKeySet(t, s)); err == nil {
		t.Error("a token from the retired key still verifies against the published set")
	}
	// A restart reads the same retire time.
	restarted := testSigner(t, second, now)
	if got := testKids(testKeySet(t, restarted)); !slices.Equal(got, []string{s.KeyID()}) {
		t.Errorf("after a restart past retire: kids = %v", got)
	}
}

// The next rotation retires the older key, whatever its retire time.
func TestRotateTwiceKeepsOnePrevious(t *testing.T) {
	first := testRotate(t, nil, false)
	second := testRotate(t, first, false)
	third := testRotate(t, second, false)
	oldest, middle := testSigner(t, first, testRotatedAt), testSigner(t, second, testRotatedAt)
	s := testSigner(t, third, testRotatedAt)
	got := testKids(testKeySet(t, s))
	if !slices.Equal(got, []string{s.KeyID(), middle.KeyID()}) || slices.Contains(got, oldest.KeyID()) {
		t.Errorf("kids = %v", got)
	}
}

// The recovery from a stolen key: at once, neither replaced key is published,
// signing uses the new key, and a token forged with a replaced key fails.
func TestRotateRetirePreviousRemovesEveryOldKeyAtOnce(t *testing.T) {
	first := testRotate(t, nil, false)
	second := testRotate(t, first, false) // a previous is live, inside its overlap
	oldest, stolen := testSigner(t, first, testRotatedAt), testSigner(t, second, testRotatedAt)
	forged, _, err := stolen.Sign(testClaims(), 0)
	if err != nil {
		t.Fatal(err)
	}

	third, err := Rotate(second, true, testRotatedAt)
	if err != nil {
		t.Fatal(err)
	}
	var stored storedKeys
	if err := json.Unmarshal(third, &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Previous != nil {
		t.Errorf("the row still holds a previous key: %+v", stored.Previous)
	}
	s := testSigner(t, third, testRotatedAt)
	got := testKids(testKeySet(t, s))
	if !slices.Equal(got, []string{s.KeyID()}) || slices.Contains(got, stolen.KeyID()) || slices.Contains(got, oldest.KeyID()) {
		t.Errorf("JWKS kids after --retire-previous = %v, want the new key %q alone", got, s.KeyID())
	}
	if s.KeyID() == stolen.KeyID() {
		t.Fatal("the retire kept the stolen key as current")
	}
	token, _, err := s.Sign(testClaims(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := testVerify(t, token, testKeySet(t, s)); err != nil {
		t.Errorf("a token from the new key: %v", err)
	}
	if _, err := testVerify(t, forged, testKeySet(t, s)); err == nil {
		t.Error("a token forged with the stolen key verifies after the retire")
	}
}

// A fault never makes a new key: every unreadable row is an error from both
// New and Rotate, and Rotate returns nothing to write over it.
func TestAnUnreadableRowNeverMakesAKey(t *testing.T) {
	good := testRotate(t, nil, false)
	var withPrev storedKeys
	if err := json.Unmarshal(testRotate(t, good, false), &withPrev); err != nil {
		t.Fatal(err)
	}
	withPrev.Previous.RetireAt = time.Time{}
	noRetire, err := json.Marshal(withPrev)
	if err != nil {
		t.Fatal(err)
	}
	p384, err := ecdsa.GenerateKey(elliptic.P384(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	p384Pub, err := marshalPublic(&p384.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	withPrev.Previous = &storedRetired{Public: p384Pub, RetireAt: testRotatedAt}
	wrongCurve, err := json.Marshal(withPrev)
	if err != nil {
		t.Fatal(err)
	}
	for name, raw := range map[string][]byte{
		"empty":             {},
		"not json":          []byte("-----BEGIN EC PRIVATE KEY-----"),
		"no current":        []byte(`{"current":""}`),
		"current not a key": []byte(`{"current":"-----BEGIN EC PRIVATE KEY-----\nAAAA\n-----END EC PRIVATE KEY-----\n"}`),
	} {
		if _, err := New(raw); err == nil {
			t.Errorf("New(%s) succeeded", name)
		}
		if next, err := Rotate(raw, false, testRotatedAt); err == nil || next != nil {
			t.Errorf("Rotate(%s) = %q, %v; want no row and an error", name, next, err)
		}
		if next, err := Rotate(raw, true, testRotatedAt); err == nil || next != nil {
			t.Errorf("Rotate(%s, drop) = %q, %v; want no row and an error", name, next, err)
		}
	}
	for name, raw := range map[string][]byte{"previous without a retire time": noRetire, "previous on the wrong curve": wrongCurve} {
		if _, err := New(raw); err == nil {
			t.Errorf("New(%s) succeeded", name)
		}
	}
}
