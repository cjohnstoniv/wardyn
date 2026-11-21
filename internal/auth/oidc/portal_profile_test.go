// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
)

func TestPortalProfile_AccessTokenChecks(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	now := h.clock.Now()
	cases := []struct {
		name   string
		mutate func(c map[string]any)
		want   string
	}{
		{"valid", func(c map[string]any) {}, ""},
		{"wrong cid", func(c map[string]any) { c["cid"] = "other-client" }, SubjectTokenCIDMismatch},
		{"cid missing", func(c map[string]any) { delete(c, "cid") }, SubjectTokenCIDMismatch},
		{"cid not a string", func(c map[string]any) { c["cid"] = []string{"client-1"} }, SubjectTokenCIDMismatch},
		{"scope missing", func(c map[string]any) { c["scp"] = []string{"other"} }, SubjectTokenScopeMissing},
		{"scp missing", func(c map[string]any) { delete(c, "scp") }, SubjectTokenScopeMissing},
		{"scp as a string", func(c map[string]any) { c["scp"] = "wardyn.run" }, SubjectTokenScopeMissing},
		{"wrong aud", func(c map[string]any) { c["aud"] = "api://other" }, SubjectTokenAudienceMismatch},
		{"aud array of one", func(c map[string]any) { c["aud"] = []string{"api://wardyn"} }, ""},
		{"two-element aud", func(c map[string]any) { c["aud"] = []string{"api://wardyn", "api://other"} }, SubjectTokenAudienceMismatch},
		{"uid missing", func(c map[string]any) { delete(c, "uid") }, SubjectTokenNoUser},
		{"uid not a string", func(c map[string]any) { c["uid"] = 17 }, SubjectTokenNoUser},
		{"expired", func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }, SubjectTokenInvalid},
		{"exp missing", func(c map[string]any) { delete(c, "exp") }, SubjectTokenInvalid},
		{"iat 90 s ahead", func(c map[string]any) { c["iat"] = now.Add(90 * time.Second).Unix() }, SubjectTokenIatFuture},
		{"nbf ahead", func(c map[string]any) { c["nbf"] = now.Add(time.Minute).Unix() }, SubjectTokenInvalid},
		{"nbf past", func(c map[string]any) { c["nbf"] = now.Add(-time.Minute).Unix() }, ""},
		{"other issuer", func(c map[string]any) { c["iss"] = "https://other.example.org" }, SubjectTokenInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := h.accessClaims()
			c.mutate(claims)
			_, r := h.verify(h.token(claims))
			if r.Reason != c.want {
				t.Fatalf("refusal = %+v, want %q", r, c.want)
			}
			if c.want != "" && r.Class != PortalRefusedToken {
				t.Fatalf("class = %v, want a token refusal", r.Class)
			}
		})
	}
	subj := h.mustAdmit(h.token(h.accessClaims()))
	if subj.Email != "" || subj.Sub != "alice@example.org" || subj.PersonID != "00u1abcd" {
		t.Fatalf("access-token subject = %+v: sub is display only and an access token carries no email", subj)
	}
}

func TestPortalProfile_DeclaredTypeMustMatchProfile(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	tok := h.token(h.accessClaims())
	for _, declared := range []string{PortalTokenTypeID, "urn:ietf:params:oauth:token-type:jwt", "", "access_token"} {
		_, r := h.pi.VerifyDetail(t.Context(), h.reg, tok, declared)
		if r.Reason != SubjectTokenTypeMismatch || r.Class != PortalRefusedRequest {
			t.Fatalf("declared %q: %+v", declared, r)
		}
	}
	if _, r := h.pi.VerifyDetail(t.Context(), h.reg, tok, PortalTokenTypeAccess); !r.OK() {
		t.Fatalf("the matching type was refused: %+v", r)
	}
	id := h.idRegistration()
	if _, r := h.pi.VerifyDetail(t.Context(), id, h.token(h.idClaims()), PortalTokenTypeAccess); r.Reason != SubjectTokenTypeMismatch {
		t.Fatalf("an ID-token portal accepted the access type: %+v", r)
	}
	if _, r := h.pi.VerifyDetail(t.Context(), id, h.token(h.idClaims()), PortalTokenTypeID); !r.OK() {
		t.Fatalf("the matching ID type was refused: %+v", r)
	}
	bad := h.reg
	bad.TokenProfile = "inferred"
	if _, r := h.pi.VerifyDetail(t.Context(), bad, tok, PortalTokenTypeAccess); r.Reason != PortalProfileUnknown {
		t.Fatalf("a profile that is not named was not refused: %+v", r)
	}
}

func TestPortalProfile_IDTokenChecks(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	reg := h.idRegistration()
	now := h.clock.Now()
	cases := []struct {
		name   string
		mutate func(c map[string]any)
		hdr    map[jose.HeaderKey]any
		want   string
	}{
		{"valid", func(c map[string]any) {}, nil, ""},
		{"valid with azp", func(c map[string]any) { c["azp"] = "client-1" }, nil, ""},
		{"valid with typ JWT", func(c map[string]any) {}, map[jose.HeaderKey]any{jose.HeaderType: "JWT"}, ""},
		{"wrong aud", func(c map[string]any) { c["aud"] = "other" }, nil, SubjectTokenAudienceMismatch},
		{"aud array of two", func(c map[string]any) { c["aud"] = []string{"client-1", "other"} }, nil, SubjectTokenAudienceMismatch},
		{"other azp", func(c map[string]any) { c["azp"] = "other" }, nil, SubjectTokenAudienceMismatch},
		{"header typ at+jwt", func(c map[string]any) {}, map[jose.HeaderKey]any{jose.HeaderType: "at+jwt"}, SubjectTokenNotIDToken},
		{"cid", func(c map[string]any) { c["cid"] = "client-1" }, nil, SubjectTokenNotIDToken},
		{"scp", func(c map[string]any) { c["scp"] = []string{"x"} }, nil, SubjectTokenNotIDToken},
		{"scope", func(c map[string]any) { c["scope"] = "x" }, nil, SubjectTokenNotIDToken},
		{"client_id", func(c map[string]any) { c["client_id"] = "client-1" }, nil, SubjectTokenNotIDToken},
		{"claim typ Bearer", func(c map[string]any) { c["typ"] = "Bearer" }, nil, SubjectTokenNotIDToken},
		{"sub missing", func(c map[string]any) { delete(c, "sub") }, nil, SubjectTokenNoUser},
		{"iat missing", func(c map[string]any) { delete(c, "iat") }, nil, SubjectTokenIatInvalid},
		{"exp missing", func(c map[string]any) { delete(c, "exp") }, nil, SubjectTokenInvalid},
		{"expired", func(c map[string]any) { c["exp"] = now.Add(-time.Minute).Unix() }, nil, SubjectTokenInvalid},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := h.idClaims()
			c.mutate(claims)
			_, r := h.verifyReg(reg, h.mint("k1", 0, claims, c.hdr))
			if r.Reason != c.want {
				t.Fatalf("refusal = %+v, want %q", r, c.want)
			}
		})
	}
	subj, r := h.verifyReg(reg, h.token(h.idClaims()))
	if !r.OK() || subj.PersonID != "00u1abcd" || subj.Email != "alice@example.org" || !subj.EmailVerified {
		t.Fatalf("ID-token subject = %+v, %+v: the person id is sub and the email is display", subj, r)
	}
}

func TestPortalProfile_AlgPinnedRS256(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	b64 := func(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }
	payload, _ := json.Marshal(h.accessClaims())
	signed := func(alg string) string {
		hdr, _ := json.Marshal(map[string]string{"alg": alg, "kid": "k1", "typ": "JWT"})
		return b64(hdr) + "." + b64(payload)
	}
	mac := hmac.New(sha256.New, []byte("secret"))
	hs := signed("HS256")
	mac.Write([]byte(hs))
	for name, tok := range map[string]string{
		"none":  signed("none") + ".",
		"HS256": hs + "." + b64(mac.Sum(nil)),
		"RS384": signed("RS384") + "." + b64([]byte("sig")),
	} {
		t.Run(name, func(t *testing.T) {
			h.wantRefusal(tok, SubjectTokenInvalid)
		})
	}
	if d, k := h.idp.hits(); d != 0 || k != 0 {
		t.Fatalf("a token with a refused algorithm reached the provider (%d discovery, %d key fetches)", d, k)
	}
	// A kid from another issuer is refused, never admitted: once the cooldown has passed the key set is re-read,
	// the kid is not in it, and the answer is a plain refusal.
	h.mustAdmit(h.token(h.accessClaims()))
	h.clock.Advance(portalKeyCooldown + time.Second)
	h.wantRefusal(h.mint("k-foreign", 1, h.accessClaims(), nil), SubjectTokenInvalid)
	// A token signed by the wrong key under a known kid fails the signature.
	h.wantRefusal(h.mint("k1", 1, h.accessClaims(), nil), SubjectTokenInvalid)
}

func TestPortalProfile_ClientCredentialsTokenRefused(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	c := h.accessClaims()
	delete(c, "uid")
	c["sub"] = "client-1" // a client-credentials token's sub is the client id
	h.wantRefusal(h.token(c), SubjectTokenNoUser)
}

func TestPortalProfile_ReservedSubOrUidRefused(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	for _, c := range []struct{ sub, uid string }{
		{"local:alice", "00u1abcd"},
		{"Service:billing", "00u1abcd"},
		{"alice@example.org", "delegate:person:xyz"},
		{"alice@example.org", " LOCAL:bob"},
		{"admin-token", "00u1abcd"},
	} {
		claims := h.accessClaims()
		claims["sub"], claims["uid"] = c.sub, c.uid
		h.wantRefusal(h.token(claims), DenialReservedPrincipal)
	}
	h.mustAdmit(h.token(h.accessClaims()))
}

func TestPortalAdmission_ClaimMatchTable(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	h.reg.RequiredClaim, h.reg.RequiredClaimValue = "tenant", "prod"
	cases := []struct {
		name  string
		value any
		set   bool
		ok    bool
	}{
		{"equal", "prod", true, true},
		{"different", "dev", true, false},
		{"missing", nil, false, false},
		{"null", nil, true, false},
		{"number", 1, true, false},
		{"boolean", true, true, false},
		{"array", []string{"prod"}, true, false},
		{"object", map[string]string{"v": "prod"}, true, false},
		{"case difference", "Prod", true, false},
		{"trailing space", "prod ", true, false},
		{"leading space", " prod", true, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			claims := h.accessClaims()
			if c.set {
				claims["tenant"] = c.value
			}
			subj, r := h.verify(h.token(claims))
			if c.ok {
				if !r.OK() || len(subj.Matched) != 1 || subj.Matched[0] != "tenant" {
					t.Fatalf("refused or unmatched: %+v %+v", subj, r)
				}
				return
			}
			if r.Reason != SubjectTokenClaimMismatch || r.Class != PortalRefusedAdmission {
				t.Fatalf("refusal = %+v, want a claim mismatch", r)
			}
		})
	}
	// A portal that sets no match admits on the token checks alone.
	h.reg.RequiredClaim, h.reg.RequiredClaimValue = "", ""
	if subj := h.mustAdmit(h.token(h.accessClaims())); len(subj.Matched) != 0 {
		t.Fatalf("matched %v with no match set", subj.Matched)
	}
}

func TestPortalAdmission_GroupOptionalWhenSet(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	// No group set: no groups claim is needed, and none is read.
	if subj := h.mustAdmit(h.token(h.accessClaims())); len(subj.Groups) != 0 {
		t.Fatalf("groups %v read with no group set", subj.Groups)
	}
	h.reg.Group = "Wardyn-Users"
	h.wantRefusal(h.token(h.accessClaims()), PortalOutsideScope)
	scalar := h.accessClaims()
	scalar["groups"] = "wardyn-users"
	h.wantRefusal(h.token(scalar), PortalOutsideScope)
	other := h.accessClaims()
	other["groups"] = []string{"eng"}
	r := h.wantRefusal(h.token(other), PortalOutsideScope)
	if r.Class != PortalRefusedAdmission {
		t.Fatalf("class = %v", r.Class)
	}
	ok := h.accessClaims()
	ok["groups"] = []string{"eng", "WARDYN-USERS"}
	if subj := h.mustAdmit(h.token(ok)); len(subj.Groups) != 2 || subj.Groups[1] != "wardyn-users" {
		t.Fatalf("groups = %v, want the canonical list", subj.Groups)
	}
	h.reg.GroupsClaim = "roles"
	h.wantRefusal(h.token(ok), PortalOutsideScope)
	named := h.accessClaims()
	named["roles"] = []string{"wardyn-users"}
	h.mustAdmit(h.token(named))
}

func TestPortalAdmission_GroupAndClaimBothRequired(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	h.reg.Group = "wardyn-users"
	h.reg.RequiredClaim, h.reg.RequiredClaimValue = "tenant", "prod"
	build := func(group, tenant bool) string {
		c := h.accessClaims()
		if group {
			c["groups"] = []string{"wardyn-users"}
		}
		if tenant {
			c["tenant"] = "prod"
		}
		return h.token(c)
	}
	h.wantRefusal(build(true, false), SubjectTokenClaimMismatch)
	h.wantRefusal(build(false, true), PortalOutsideScope)
	h.wantRefusal(build(false, false), SubjectTokenClaimMismatch)
	subj := h.mustAdmit(build(true, true))
	if len(subj.Matched) != 1 || len(subj.Groups) != 1 {
		t.Fatalf("subject = %+v", subj)
	}
}

func TestPortalFreshness_StaleAndFutureIatRefused(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	now := h.clock.Now()
	at := func(iat time.Duration) map[string]any {
		c := h.accessClaims()
		c["iat"] = now.Add(iat).Unix()
		return c
	}
	h.mustAdmit(h.token(at(-295 * time.Second)))
	h.wantRefusal(h.token(at(-301*time.Second)), SubjectTokenStale)
	h.wantRefusal(h.token(at(-300*time.Second)), SubjectTokenStale) // exactly max age: no life left
	h.wantRefusal(h.token(at(-296*time.Second)), SubjectTokenStale) // under 5 s of life left
	h.mustAdmit(h.token(at(-295 * time.Second)))
	h.wantRefusal(h.token(at(61*time.Second)), SubjectTokenIatFuture)
	h.mustAdmit(h.token(at(59 * time.Second)))
	for name, v := range map[string]any{"string": "1700000000", "word": "soon", "overflow": 1e30, "negative": -5, "array": []int{1}, "null": nil, "bool": true} {
		t.Run("iat "+name, func(t *testing.T) {
			c := h.accessClaims()
			c["iat"] = v
			h.wantRefusal(h.token(c), SubjectTokenIatInvalid)
		})
	}
	c := h.accessClaims()
	delete(c, "iat")
	h.wantRefusal(h.token(c), SubjectTokenIatInvalid)
	// A token with under 5 s to its exp has no grant to give.
	short := h.accessClaims()
	short["exp"] = now.Add(3 * time.Second).Unix()
	h.wantRefusal(h.token(short), SubjectTokenStale)
	// The maximum age is the portal's own.
	h.reg.MaxSubjectTokenAge = 600 * time.Second
	h.mustAdmit(h.token(at(-400 * time.Second)))
	h.wantRefusal(h.token(at(-601*time.Second)), SubjectTokenStale)
}

func TestPortalFreshness_ProviderClockAheadExtendsByLead(t *testing.T) {
	// The provider's clock runs 50 s ahead, so a token carries an iat 50 s ahead of Wardyn's clock at issue time.
	// It is accepted at once, and stays acceptable until Wardyn's clock reaches iat plus the maximum age: the
	// bound is the maximum age plus the provider's lead, measured from the real issue time.
	h := newPortalHarness(t, "example.com", true, "")
	issued := h.clock.Now()
	c := h.accessClaims()
	c["iat"] = issued.Add(50 * time.Second).Unix()
	c["exp"] = issued.Add(20 * time.Minute).Unix()
	tok := h.token(c)
	h.mustAdmit(tok)
	h.clock.Advance(340 * time.Second) // 340 s after the real issue time: past the maximum age, inside the lead
	if subj := h.mustAdmit(tok); subj.GrantEnd.Sub(issued) != 350*time.Second {
		t.Fatalf("grant end %v after issue, want 350 s (max age plus lead)", subj.GrantEnd.Sub(issued))
	}
	h.clock.Advance(6 * time.Second) // under 5 s left
	h.wantRefusal(tok, SubjectTokenStale)
}

func TestPortalFreshness_SignedEndIsEarlierOfIatPlusMaxAgeAndExp(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "")
	now := h.clock.Now()
	for _, c := range []struct {
		name     string
		iat, exp time.Duration
		wantEnd  time.Duration
	}{
		{"exp first", -10 * time.Second, 100 * time.Second, 100 * time.Second},
		{"iat plus max age first", -10 * time.Second, 20 * time.Minute, 290 * time.Second},
		{"iat 50 s ahead", 50 * time.Second, 20 * time.Minute, 350 * time.Second},
		{"iat 50 s ahead, exp first", 50 * time.Second, 120 * time.Second, 120 * time.Second},
		{"equal", -100 * time.Second, 200 * time.Second, 200 * time.Second},
	} {
		t.Run(c.name, func(t *testing.T) {
			claims := h.accessClaims()
			claims["iat"], claims["exp"] = now.Add(c.iat).Unix(), now.Add(c.exp).Unix()
			subj := h.mustAdmit(h.token(claims))
			if got := subj.GrantEnd.Sub(now); got != c.wantEnd {
				t.Fatalf("GrantEnd = now+%v, want now+%v", got, c.wantEnd)
			}
			if subj.IssuedAt.Unix() != now.Add(c.iat).Unix() || subj.Expiry.Unix() != now.Add(c.exp).Unix() {
				t.Fatalf("subject times = %v, %v", subj.IssuedAt, subj.Expiry)
			}
		})
	}
}

func TestPortalProfile_ListClaimsNeedEveryElementAString(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	for name, scp := range map[string]any{
		"null element":   []any{"wardyn.run", nil},
		"number element": []any{"wardyn.run", 5},
		"object element": []any{"wardyn.run", map[string]string{"a": "b"}},
		"null only":      []any{nil},
	} {
		t.Run("scp "+name, func(t *testing.T) {
			c := h.accessClaims()
			c["scp"] = scp
			h.wantRefusal(h.token(c), SubjectTokenScopeMissing)
		})
	}
	for name, aud := range map[string]any{"null element": []any{nil}, "one null and the audience": []any{"api://wardyn", nil}} {
		t.Run("aud "+name, func(t *testing.T) {
			c := h.accessClaims()
			c["aud"] = aud
			h.wantRefusal(h.token(c), SubjectTokenAudienceMismatch)
		})
	}
	c := h.accessClaims()
	c["scp"] = []any{"wardyn.run", "extra"}
	h.mustAdmit(h.token(c))
}

func TestPortalProfile_IDTokenTypHeaderPresence(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	reg := h.idRegistration()
	for name, typ := range map[string]any{"empty string": "", "boolean": true, "number": 5, "lower case": "jwt", "other": "at+jwt"} {
		t.Run(name, func(t *testing.T) {
			_, r := h.verifyReg(reg, h.mint("k1", 0, h.idClaims(), map[jose.HeaderKey]any{jose.HeaderType: typ}))
			if r.Reason != SubjectTokenNotIDToken {
				t.Fatalf("typ %v: %+v, want a refusal as not an ID token", typ, r)
			}
		})
	}
	// A JSON null typ is present, and not the string JWT: go-jose drops it from the parsed header, so the raw
	// protected header is what is read.
	for name, hdr := range map[string]string{
		"null":       `{"alg":"RS256","kid":"k1","typ":null}`,
		"null first": `{"typ":null,"alg":"RS256","kid":"k1"}`,
		"object":     `{"alg":"RS256","kid":"k1","typ":{}}`,
		"array":      `{"alg":"RS256","kid":"k1","typ":["JWT"]}`,
	} {
		t.Run("raw "+name, func(t *testing.T) {
			if _, r := h.verifyReg(reg, h.mintHeader(0, h.idClaims(), hdr)); r.Reason != SubjectTokenNotIDToken {
				t.Fatalf("header %s: %+v, want a refusal as not an ID token", hdr, r)
			}
		})
	}
	if _, r := h.verifyReg(reg, h.mintHeader(0, h.idClaims(), `{"alg":"RS256","kid":"k1","typ":"JWT"}`)); !r.OK() {
		t.Fatalf("a raw typ JWT was refused: %+v", r)
	}
	if _, r := h.verifyReg(reg, h.mintHeader(0, h.idClaims(), `{"alg":"RS256","kid":"k1"}`)); !r.OK() {
		t.Fatalf("a raw absent typ was refused: %+v", r)
	}
	if _, r := h.verifyReg(reg, h.token(h.idClaims())); !r.OK() {
		t.Fatalf("an absent typ was refused: %+v", r)
	}
	if _, r := h.verifyReg(reg, h.mint("k1", 0, h.idClaims(), map[jose.HeaderKey]any{jose.HeaderType: "JWT"})); !r.OK() {
		t.Fatalf("typ JWT was refused: %+v", r)
	}
	// The access-token profile never reads the header.
	h.mustAdmit(h.mint("k1", 0, h.accessClaims(), map[jose.HeaderKey]any{jose.HeaderType: true}))
	h.mustAdmit(h.mint("k1", 0, h.accessClaims(), map[jose.HeaderKey]any{jose.HeaderType: "at+jwt"}))
	h.mustAdmit(h.mintHeader(0, h.accessClaims(), `{"alg":"RS256","kid":"k1","typ":null}`))
}

func TestPortalAdmission_IDTokenEmailDomain(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	reg := h.idRegistration()
	reg.EmailDomains = []string{"example.org"}
	with := func(mutate func(c map[string]any)) string {
		c := h.idClaims()
		mutate(c)
		return h.token(c)
	}
	cases := []struct {
		name   string
		mutate func(c map[string]any)
		want   string
	}{
		{"address in the list", func(c map[string]any) {}, ""},
		{"domain in another case", func(c map[string]any) { c["email"] = "Alice@EXAMPLE.org" }, ""},
		{"address out of the list", func(c map[string]any) { c["email"] = "alice@other.example" }, DenialEmailDomain},
		{"subdomain is not the domain", func(c map[string]any) { c["email"] = "alice@sub.example.org" }, DenialEmailDomain},
		{"look-alike non-ASCII domain", func(c map[string]any) { c["email"] = "alice@examplе.org" }, DenialEmailDomain},
		{"no email", func(c map[string]any) { delete(c, "email") }, DenialEmailDomain},
		{"email not a string", func(c map[string]any) { c["email"] = 5 }, DenialEmailDomain},
		{"email_verified absent", func(c map[string]any) { delete(c, "email_verified") }, DenialEmailVerifiedAbsent},
		{"email_verified false", func(c map[string]any) { c["email_verified"] = false }, DenialEmailUnverified},
		{"email_verified as a string", func(c map[string]any) { c["email_verified"] = "true" }, DenialEmailUnverified},
		{"email_verified a number", func(c map[string]any) { c["email_verified"] = 1 }, DenialEmailUnverified},
		{"email_verified null", func(c map[string]any) { c["email_verified"] = nil }, DenialEmailUnverified},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			subj, r := h.verifyReg(reg, with(c.mutate))
			if r.Reason != c.want {
				t.Fatalf("refusal = %+v, want %q", r, c.want)
			}
			if c.want == "" && (subj.Email != "alice@example.org" && c.name == "address in the list" || !subj.EmailVerified) {
				t.Fatalf("subject = %+v", subj)
			}
			if c.want != "" && r.Class != PortalRefusedAdmission {
				t.Fatalf("class = %v, want an admission refusal", r.Class)
			}
		})
	}
	// No rule configured: neither field is needed.
	none := h.idRegistration()
	if _, r := h.verifyReg(none, with(func(c map[string]any) { delete(c, "email_verified"); c["email"] = "x@elsewhere.test" })); !r.OK() {
		t.Fatalf("a portal with no domain list refused on email: %+v", r)
	}
	// The access-token profile has no verified email and never reads these claims, even with a list set.
	acc := h.reg
	acc.EmailDomains = []string{"example.org"}
	c := h.accessClaims()
	c["email"], c["email_verified"] = "x@elsewhere.test", false
	subj, r := h.verifyReg(acc, h.token(c))
	if !r.OK() || subj.Email != "" || subj.EmailVerified {
		t.Fatalf("access-token subject = %+v, %+v: the email field must not be read", subj, r)
	}
}

func TestPortalProfile_InvalidRegistrationRefused(t *testing.T) {
	h := newPortalHarness(t, "example.com", true, "/oauth2/a")
	tok := h.token(h.accessClaims())
	for name, mod := range map[string]func(r *PortalRegistration){
		"no client id":         func(r *PortalRegistration) { r.ClientID = "" },
		"no issuer":            func(r *PortalRegistration) { r.Issuer = "" },
		"access, no audience":  func(r *PortalRegistration) { r.Audience = "" },
		"access, no scopes":    func(r *PortalRegistration) { r.RequiredScopes = nil },
		"access, empty scopes": func(r *PortalRegistration) { r.RequiredScopes = []string{} },
	} {
		t.Run(name, func(t *testing.T) {
			reg := h.reg
			mod(&reg)
			_, r := h.verifyReg(reg, tok)
			if r.Reason != PortalRegistrationInvalid || r.Class != PortalRefusedRequest {
				t.Fatalf("refusal = %+v, want %s as a request refusal", r, PortalRegistrationInvalid)
			}
		})
	}
	if disc, keys := h.idp.hits(); disc != 0 || keys != 0 {
		t.Fatalf("an invalid registration reached the network: (%d, %d)", disc, keys)
	}
	// An ID-token registration needs a client id only: no audience and no scopes.
	id := h.idRegistration()
	if _, r := h.verifyReg(id, h.token(h.idClaims())); !r.OK() {
		t.Fatalf("a valid ID-token registration was refused: %+v", r)
	}
	id.ClientID = ""
	if _, r := h.verifyReg(id, h.token(h.idClaims())); r.Reason != PortalRegistrationInvalid {
		t.Fatalf("an ID-token registration with no client id: %+v", r)
	}
	h.mustAdmit(tok)
}
