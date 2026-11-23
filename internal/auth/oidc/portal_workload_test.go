// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

const (
	workloadTestAudience = "https://wardyn.example.com"
	workloadTestSubject  = "repo:org/app:ref:refs/heads/main"
)

// workloadClaims are the claims of a valid workload token, issued at the harness clock.
func workloadClaims(h *portalHarness) map[string]any {
	now := h.clock.Now()
	return map[string]any{
		"iss": h.reg.Issuer, "aud": workloadTestAudience, "sub": workloadTestSubject,
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

// workloadReg is the inbound trust that workloadClaims satisfies.
func workloadReg(h *portalHarness) WorkloadRegistration {
	return WorkloadRegistration{Issuer: h.reg.Issuer, Audience: workloadTestAudience, Subject: workloadTestSubject}
}

func verifyWorkload(h *portalHarness, reg WorkloadRegistration, raw string) (WorkloadSubject, PortalRefusal) {
	return h.pi.VerifyWorkload(context.Background(), reg, raw)
}

func wantWorkloadAdmit(t *testing.T, h *portalHarness, reg WorkloadRegistration, raw string) WorkloadSubject {
	t.Helper()
	subj, r := verifyWorkload(h, reg, raw)
	if !r.OK() {
		t.Fatalf("workload token refused: %+v", r)
	}
	return subj
}

func wantWorkloadRefusal(t *testing.T, h *portalHarness, reg WorkloadRegistration, raw, reason string) PortalRefusal {
	t.Helper()
	subj, r := verifyWorkload(h, reg, raw)
	if r.Reason != reason {
		t.Fatalf("refusal = %+v, want reason %q", r, reason)
	}
	if !subj.IssuedAt.IsZero() || subj.Subject != "" {
		t.Fatalf("a refused token returned a subject: %+v", subj)
	}
	return r
}

func TestWorkload_Admits(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	c := workloadClaims(h)
	subj := wantWorkloadAdmit(t, h, workloadReg(h), h.token(c))
	if subj.Subject != workloadTestSubject || subj.Issuer != h.reg.Issuer {
		t.Fatalf("subject = %+v, want sub %q and issuer %q", subj, workloadTestSubject, h.reg.Issuer)
	}
	if subj.IssuedAt.Unix() != c["iat"].(int64) || subj.Expiry.Unix() != c["exp"].(int64) {
		t.Fatalf("times = (%v, %v), want the token's iat and exp", subj.IssuedAt, subj.Expiry)
	}
	if len(subj.Matched) != 0 {
		t.Fatalf("Matched = %v with no claim set", subj.Matched)
	}
}

func TestWorkload_RS256Only(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	payload, _ := json.Marshal(workloadClaims(h))
	hdr, _ := json.Marshal(map[string]string{"alg": "none", "kid": "k1", "typ": "JWT"})
	none := b64(hdr) + "." + b64(payload) + "."

	// HS256 keyed with the provider's own public key, the classic confusion of a key set's bytes for a secret.
	der, err := x509.MarshalPKIXPublicKey(&portalTestKeys(t)[0].PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	secret := pem.EncodeToMemory(&pem.Block{Type: "PUBLIC KEY", Bytes: der})
	hs := signWorkload(t, jose.SigningKey{Algorithm: jose.HS256, Key: secret}, payload)

	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	es := signWorkload(t, jose.SigningKey{Algorithm: jose.ES256, Key: jose.JSONWebKey{Key: ec, KeyID: "k1"}}, payload)

	for name, tok := range map[string]string{"none": none, "HS256": hs, "ES256": es} {
		t.Run(name, func(t *testing.T) {
			wantWorkloadRefusal(t, h, workloadReg(h), tok, SubjectTokenInvalid)
		})
	}
	if d, k := h.idp.hits(); d != 0 || k != 0 {
		t.Fatalf("a token with a refused algorithm reached the provider (%d discovery, %d key fetches)", d, k)
	}
}

func signWorkload(t *testing.T, key jose.SigningKey, payload []byte) string {
	t.Helper()
	signer, err := jose.NewSigner(key, nil)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := signer.Sign(payload)
	if err != nil {
		t.Fatal(err)
	}
	s, err := obj.CompactSerialize()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestWorkload_IssuerExactBytes(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	right := h.reg.Issuer
	other := "https://other.example.org"
	now := h.clock.Now()
	body := func(issMembers string) []byte {
		return []byte(fmt.Sprintf(`{%s,"aud":%q,"sub":%q,"iat":%d,"exp":%d}`,
			issMembers, workloadTestAudience, workloadTestSubject, now.Unix(), now.Add(5*time.Minute).Unix()))
	}
	for name, members := range map[string]string{
		"trailing slash":   fmt.Sprintf(`"iss":%q`, right+"/"),
		"other case":       fmt.Sprintf(`"iss":%q`, strings.ToUpper(right)),
		"only ISS":         fmt.Sprintf(`"ISS":%q`, right),
		"iss first, ISS":   fmt.Sprintf(`"iss":%q,"ISS":%q`, right, other),
		"no issuer at all": `"x":1`,
	} {
		t.Run(name, func(t *testing.T) {
			wantWorkloadRefusal(t, h, workloadReg(h), h.mintRaw("k1", 0, body(members), nil), SubjectTokenInvalid)
		})
	}
	// go-oidc fills Issuer by Go's case-insensitive struct decode, where the later member wins. With iss first and
	// ISS last it reads the other issuer, so the token is refused, by go-oidc's own issuer check or by the exact
	// Issuer comparison after it; which of the two does not matter. With ISS first and iss last both readings are
	// the issuer's own signed value, and the exact iss member agrees.
	wantWorkloadAdmit(t, h, workloadReg(h), h.mintRaw("k1", 0, body(fmt.Sprintf(`"ISS":%q,"iss":%q`, other, right)), nil))
}

func TestWorkload_AudienceExactlyOne(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	for name, aud := range map[string]any{
		"two values":     []string{workloadTestAudience, "https://other.example.com"},
		"another string": "https://other.example.com",
		"missing":        nil,
		"empty list":     []string{},
	} {
		t.Run(name, func(t *testing.T) {
			c := workloadClaims(h)
			if aud == nil {
				delete(c, "aud")
			} else {
				c["aud"] = aud
			}
			wantWorkloadRefusal(t, h, workloadReg(h), h.token(c), SubjectTokenAudienceMismatch)
		})
	}
	c := workloadClaims(h)
	c["aud"] = []string{workloadTestAudience}
	wantWorkloadAdmit(t, h, workloadReg(h), h.token(c))
}

func TestWorkload_SubjectExact(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	for name, sub := range map[string]any{
		"prefix":     "x" + workloadTestSubject,
		"suffix":     workloadTestSubject + "x",
		"other case": strings.ToUpper(workloadTestSubject),
		"missing":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			c := workloadClaims(h)
			if sub == nil {
				delete(c, "sub")
			} else {
				c["sub"] = sub
			}
			wantWorkloadRefusal(t, h, workloadReg(h), h.token(c), SubjectTokenSubjectMismatch)
		})
	}
	t.Run("not string", func(t *testing.T) {
		c := workloadClaims(h)
		c["sub"] = []string{workloadTestSubject}
		// The provider library's claim decode refuses a non-string sub before the subject check runs.
		if r := wantWorkloadRefusal(t, h, workloadReg(h), h.token(c), SubjectTokenInvalid); r.Class != PortalRefusedToken {
			t.Fatalf("refusal = %+v, want the token class", r)
		}
	})
}

func TestWorkload_RequiredClaim(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	reg := workloadReg(h)
	reg.RequiredClaim, reg.RequiredClaimValue = "repository", "org/app"
	for name, v := range map[string]any{
		"number":        7,
		"list":          []string{"org/app"},
		"another value": "org/other",
		"absent":        nil,
	} {
		t.Run(name, func(t *testing.T) {
			c := workloadClaims(h)
			if v != nil {
				c["repository"] = v
			}
			wantWorkloadRefusal(t, h, reg, h.token(c), SubjectTokenClaimMismatch)
		})
	}
	c := workloadClaims(h)
	c["repository"] = "org/app"
	subj := wantWorkloadAdmit(t, h, reg, h.token(c))
	if !slices.Equal(subj.Matched, []string{"repository"}) {
		t.Fatalf("Matched = %v, want [repository]", subj.Matched)
	}
}

func TestWorkload_AgeRulePerTrust(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	now := h.clock.Now()
	// Every token here has exp now + 5 min; the signed end is the earlier of iat plus the age and exp, and must
	// leave portalMinGrantLife.
	at := func(iat time.Duration) string {
		c := workloadClaims(h)
		c["iat"] = now.Add(iat).Unix()
		return h.token(c)
	}
	for _, tc := range []struct {
		age            time.Duration
		admit, refuses []time.Duration
	}{
		{0, []time.Duration{295 * time.Second}, []time.Duration{296 * time.Second, 301 * time.Second}},
		{3600 * time.Second, []time.Duration{3595 * time.Second}, []time.Duration{3596 * time.Second, 3601 * time.Second}},
	} {
		reg := workloadReg(h)
		reg.MaxTokenAge = tc.age
		for _, d := range tc.admit {
			wantWorkloadAdmit(t, h, reg, at(-d))
		}
		for _, d := range tc.refuses {
			wantWorkloadRefusal(t, h, reg, at(-d), SubjectTokenStale)
		}
	}
	reg := workloadReg(h)
	wantWorkloadRefusal(t, h, reg, at(61*time.Second), SubjectTokenIatFuture)
	c := workloadClaims(h)
	delete(c, "iat")
	wantWorkloadRefusal(t, h, reg, h.token(c), SubjectTokenIatInvalid)
	c = workloadClaims(h)
	c["nbf"] = now.Add(time.Minute).Unix()
	wantWorkloadRefusal(t, h, reg, h.token(c), SubjectTokenInvalid)
	c = workloadClaims(h)
	c["iat"], c["exp"] = now.Add(-2*time.Minute).Unix(), now.Add(-time.Minute).Unix()
	wantWorkloadRefusal(t, h, reg, h.token(c), SubjectTokenInvalid)
}

func TestWorkload_RegistrationInvalid(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	tok := h.token(workloadClaims(h))
	for name, mod := range map[string]func(*WorkloadRegistration){
		"empty issuer":        func(r *WorkloadRegistration) { r.Issuer = "" },
		"empty audience":      func(r *WorkloadRegistration) { r.Audience = "" },
		"empty subject":       func(r *WorkloadRegistration) { r.Subject = "" },
		"negative age":        func(r *WorkloadRegistration) { r.MaxTokenAge = -time.Second },
		"age past the bound":  func(r *WorkloadRegistration) { r.MaxTokenAge = 3601 * time.Second },
		"claim with no value": func(r *WorkloadRegistration) { r.RequiredClaim = "repository" },
		"value with no claim": func(r *WorkloadRegistration) { r.RequiredClaimValue = "org/app" },
	} {
		t.Run(name, func(t *testing.T) {
			reg := workloadReg(h)
			mod(&reg)
			if r := wantWorkloadRefusal(t, h, reg, tok, PortalRegistrationInvalid); r.Class != PortalRefusedRequest {
				t.Fatalf("refusal = %+v, want the request class", r)
			}
		})
	}
	if d, k := h.idp.hits(); d != 0 || k != 0 {
		t.Fatalf("an invalid registration cost (%d,%d) discovery and key fetches, want none", d, k)
	}
}

func TestWorkload_UnknownKidCooldown(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	wantWorkloadAdmit(t, h, workloadReg(h), h.token(workloadClaims(h)))
	_, warm := h.idp.hits()
	h.clock.Advance(portalKeyCooldown + time.Second)
	for i := range 50 {
		h.clock.Advance(500 * time.Millisecond) // 25 s in all: inside one cooldown
		if _, r := verifyWorkload(h, workloadReg(h), h.mint(fmt.Sprintf("unknown-%d", i), 1, workloadClaims(h), nil)); r.OK() {
			t.Fatalf("token %d with an unknown kid was admitted", i)
		}
	}
	_, burst := h.idp.hits()
	if burst-warm > 1 {
		t.Fatalf("50 unknown kids in one cooldown cost %d key fetches, want at most 1", burst-warm)
	}
	h.clock.Advance(portalKeyCooldown + time.Second)
	if _, r := verifyWorkload(h, workloadReg(h), h.mint("unknown-after", 1, workloadClaims(h), nil)); r.OK() {
		t.Fatal("a token with an unknown kid was admitted after the cooldown")
	}
	h.wantKeyHits(burst + 1)
}

func TestWorkload_IssuerDownFailsClosed(t *testing.T) {
	wantDown := func(t *testing.T, h *portalHarness) {
		t.Helper()
		subj, r := verifyWorkload(h, workloadReg(h), h.token(workloadClaims(h)))
		if r.OK() || r.Class != PortalRefusedUnavailable || r.Reason != PortalIssuerUnavailable || r.RetryAfter <= 0 {
			t.Fatalf("refusal = %+v (subject %+v), want an unavailable refusal with a Retry-After", r, subj)
		}
	}
	t.Run("discovery down", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		h.idp.set(func(f *fakeIdP) { f.discStatus = 500 })
		wantDown(t, h)
	})
	t.Run("keys down", func(t *testing.T) {
		h := newPortalHarness(t, "example.com", true, "/oauth2/a")
		wantWorkloadAdmit(t, h, workloadReg(h), h.token(workloadClaims(h)))
		h.clock.Advance(time.Hour)
		h.idp.set(func(f *fakeIdP) { f.keyStatus = 500 })
		wantDown(t, h)
	})
}

func TestWorkload_SharesThePortalVerifier(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	h.mustAdmit(h.token(h.accessClaims()))
	disc, keys := h.idp.hits()
	wantWorkloadAdmit(t, h, workloadReg(h), h.token(workloadClaims(h)))
	if d, k := h.idp.hits(); d != disc || k != keys {
		t.Fatalf("the workload call went from (%d,%d) to (%d,%d) fetches, want the portal's warm verifier", disc, keys, d, k)
	}
}

func TestWorkloadCandidate(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	right := h.reg.Issuer
	iss, aud, sub, ok := WorkloadCandidate(h.token(workloadClaims(h)))
	if !ok || iss != right || aud != workloadTestAudience || sub != workloadTestSubject {
		t.Fatalf("candidate = (%q, %q, %q, %v)", iss, aud, sub, ok)
	}
	body := func(issMembers string) []byte {
		return []byte(fmt.Sprintf(`{%s,"aud":%q,"sub":%q}`, issMembers, workloadTestAudience, workloadTestSubject))
	}
	for _, members := range []string{
		fmt.Sprintf(`"iss":%q,"ISS":"https://other.example.org"`, right),
		fmt.Sprintf(`"ISS":"https://other.example.org","iss":%q`, right),
	} {
		if iss, _, _, ok := WorkloadCandidate(h.mintRaw("k1", 0, body(members), nil)); !ok || iss != right {
			t.Fatalf("members %s: iss = %q, ok %v; want the exact iss member", members, iss, ok)
		}
	}
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	good := strings.Split(h.token(workloadClaims(h)), ".")
	hsHdr, _ := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	twoSig := fmt.Sprintf(`{"payload":%q,"signatures":[{"protected":%q,"signature":%q},{"protected":%q,"signature":%q}]}`,
		good[1], good[0], good[2], good[0], good[2])
	refused := map[string]string{
		"HS256 header":   b64(hsHdr) + "." + good[1] + "." + good[2],
		"two signatures": twoSig,
		"garbage":        "not.a.token",
	}
	for name, mod := range map[string]func(map[string]any){
		"two audiences":  func(c map[string]any) { c["aud"] = []string{workloadTestAudience, "https://other.example.com"} },
		"empty iss":      func(c map[string]any) { c["iss"] = "" },
		"empty sub":      func(c map[string]any) { c["sub"] = "" },
		"non-string sub": func(c map[string]any) { c["sub"] = 7 },
	} {
		c := workloadClaims(h)
		mod(c)
		refused[name] = h.token(c)
	}
	for name, raw := range refused {
		t.Run(name, func(t *testing.T) {
			if iss, aud, sub, ok := WorkloadCandidate(raw); ok || iss != "" || aud != "" || sub != "" {
				t.Fatalf("candidate = (%q, %q, %q, %v), want nothing", iss, aud, sub, ok)
			}
		})
	}
	if d, k := h.idp.hits(); d != 0 || k != 0 {
		t.Fatalf("WorkloadCandidate cost (%d,%d) discovery and key fetches, want none", d, k)
	}
}
