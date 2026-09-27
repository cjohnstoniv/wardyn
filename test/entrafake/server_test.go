// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package entrafake_test

import (
	"context"
	"encoding/json"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"testing"

	gooidc "github.com/coreos/go-oidc/v3/oidc"

	"github.com/cjohnstoniv/wardyn/test/entrafake"
)

// noRedirectClient follows nothing: every leg of this flow answers with a 302
// whose Location IS the assertion, and a client that followed it would dial the
// fake's own redirect URI (a port nothing is listening on).
func noRedirectClient() *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// authorize drives one /authorize leg and returns the Location query it
// redirected to.
func authorize(t *testing.T, s *entrafake.Server, state, verifier string, scopes ...string) url.Values {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, s.AuthorizeURL(state, entrafake.Challenge(verifier), "n-"+state, scopes...), nil)
	if err != nil {
		t.Fatalf("build authorize request: %v", err)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusFound {
		t.Fatalf("authorize: status %d, want 302", resp.StatusCode)
	}
	loc, err := url.Parse(resp.Header.Get("Location"))
	if err != nil {
		t.Fatalf("parse Location: %v", err)
	}
	return loc.Query()
}

// postToken posts a form to /token and returns the status plus the decoded body.
func postToken(t *testing.T, s *entrafake.Server, form url.Values) (int, map[string]any) {
	t.Helper()
	form.Set("client_id", s.ClientID())
	resp, err := http.PostForm(s.TokenURL(), form)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body := map[string]any{}
	if err := decodeJSON(resp, &body); err != nil {
		t.Fatalf("decode token response: %v", err)
	}
	return resp.StatusCode, body
}

func decodeJSON(resp *http.Response, into any) error {
	return json.NewDecoder(resp.Body).Decode(into)
}

// redeemCode runs authorize + the authorization_code grant and returns the body.
func redeemCode(t *testing.T, s *entrafake.Server, verifier string, scopes ...string) map[string]any {
	t.Helper()
	q := authorize(t, s, "st-1", verifier, scopes...)
	if q.Get("error") != "" {
		t.Fatalf("authorize refused: %s / %s", q.Get("error"), q.Get("error_description"))
	}
	status, body := postToken(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"code_verifier": {verifier},
		"redirect_uri":  {s.RedirectURI()},
	})
	if status != http.StatusOK {
		t.Fatalf("code redemption: status %d body %v", status, body)
	}
	return body
}

// TestIDTokenVerifiesThroughDiscovery is the fake's floor: a real go-oidc
// verifier, pointed at nothing but the issuer URL, must resolve discovery, fetch
// the JWKS and verify the signed id_token — including the audience and the
// nonce the authorization request carried. Without this every identity
// assertion the sign-in lane makes is against a token nothing checked.
func TestIDTokenVerifiesThroughDiscovery(t *testing.T) {
	s := entrafake.New()
	defer s.Close()

	body := redeemCode(t, s, "verifier-0123456789012345678901234567890123456789",
		"openid", "offline_access", s.ConsentedScopes()[1])

	raw, ok := body["id_token"].(string)
	if !ok || raw == "" {
		t.Fatalf("no id_token in %v", body)
	}
	provider, err := gooidc.NewProvider(context.Background(), s.Issuer())
	if err != nil {
		t.Fatalf("discovery: %v", err)
	}
	tok, err := provider.Verifier(&gooidc.Config{ClientID: s.ClientID()}).Verify(context.Background(), raw)
	if err != nil {
		t.Fatalf("verify id_token: %v", err)
	}
	if tok.Subject != s.Subject() {
		t.Fatalf("sub = %q, want %q", tok.Subject, s.Subject())
	}
	if tok.Nonce != "n-st-1" {
		t.Fatalf("nonce = %q, want the one the authorization request carried", tok.Nonce)
	}
	var claims struct {
		TenantID string `json:"tid"`
	}
	if err := tok.Claims(&claims); err != nil {
		t.Fatalf("claims: %v", err)
	}
	if claims.TenantID != s.TenantID() {
		t.Fatalf("tid = %q, want %q", claims.TenantID, s.TenantID())
	}
}

// TestCodeChallengeMismatchIsRefused is the PKCE property: a code redeemed with
// a verifier that does not hash to the challenge the authorization request
// carried is invalid_grant. A fake that skipped this would let a capture lane
// pass its own PKCE test by sending any string at all.
func TestCodeChallengeMismatchIsRefused(t *testing.T) {
	s := entrafake.New()
	defer s.Close()

	q := authorize(t, s, "st-2", "verifier-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "openid", "offline_access")
	if q.Get("code") == "" {
		t.Fatalf("no code: %v", q)
	}
	status, body := postToken(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"code_verifier": {"verifier-bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"},
	})
	if status == http.StatusOK {
		t.Fatalf("a mismatched code_verifier was ACCEPTED: %v", body)
	}
	if got := body["error"]; got != entrafake.ErrInvalidGrant {
		t.Fatalf("error = %v, want %q", got, entrafake.ErrInvalidGrant)
	}
}

// TestCodeIsSingleUse: a code is spent on the first redemption attempt, right
// or wrong, so a replay cannot brute-force the verifier.
func TestCodeIsSingleUse(t *testing.T) {
	s := entrafake.New()
	defer s.Close()

	verifier := "verifier-cccccccccccccccccccccccccccccccccccccccc"
	q := authorize(t, s, "st-3", verifier, "openid", "offline_access")
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"code_verifier": {verifier},
	}
	if status, body := postToken(t, s, form); status != http.StatusOK {
		t.Fatalf("first redemption: status %d body %v", status, body)
	}
	status, body := postToken(t, s, form)
	if status == http.StatusOK {
		t.Fatalf("the code was redeemable TWICE: %v", body)
	}
	if got := body["error"]; got != entrafake.ErrInvalidGrant {
		t.Fatalf("error = %v, want %q", got, entrafake.ErrInvalidGrant)
	}
}

// TestRefreshIgnoresTheRequestedSubset pins ENTRA'S OWN BEHAVIOUR, which is not
// what an OAuth reader expects: a refresh grant naming a SUBSET of the
// consented scopes comes back with a token carrying EVERY consented scope, and
// the response's granted `scope` lists all of them.
//
// This is measured against a real tenant, not inferred — requesting one
// `vso.*` scope and requesting two both returned the whole consented set. It is
// pinned here because the opposite belief is load-bearing if anyone holds it:
// a caller that thinks the token is narrowed will think the token bounds what a
// run can do, and it does not. The token is the person's own identity with
// everything they consented to; narrowing has to come from the party in front
// of the resource.
//
// Asking for the subset anyway is correct and is what the capture does: it is
// harmless, it is what a well-behaved client sends, and it is what would
// narrow if this ever changes.
func TestRefreshIgnoresTheRequestedSubset(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	all := s.ConsentedScopes()
	full := append([]string{"openid", "offline_access"}, all...)

	first := redeemCode(t, s, "verifier-dddddddddddddddddddddddddddddddddddddddd", full...)
	refresh, _ := first["refresh_token"].(string)

	subset := []string{all[1], all[2]}
	status, body := postToken(t, s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
		"scope":         {strings.Join(subset, " ")},
	})
	if status != http.StatusOK {
		t.Fatalf("subset refresh: status %d body %v", status, body)
	}
	granted, _ := body["scope"].(string)
	for _, want := range full {
		if !strings.Contains(granted, want) {
			t.Fatalf("granted scope %q does not carry %q — the fake narrowed a token this service does not narrow", granted, want)
		}
	}
	access, _ := body["access_token"].(string)
	carried, ok := s.ScopesForAccessToken(access)
	if !ok {
		t.Fatal("the fake does not recognise the access token it just issued")
	}
	if len(carried) != len(full) {
		t.Fatalf("the access token carries %v; want every consented scope %v", carried, full)
	}
	// And the consent is unchanged, so a later redemption naming a scope this
	// request left out is still served.
	rotated, _ := body["refresh_token"].(string)
	status, body = postToken(t, s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {rotated},
		"scope":         {all[0]},
	})
	if status != http.StatusOK {
		t.Fatalf("a later redemption was refused: status %d body %v", status, body)
	}
}

// TestUnconsentedScopeIsRefused: asking for a scope the app registration does
// not hold is a consent refusal carrying AADSTS65001, on BOTH legs — the
// authorization request and the refresh. The capture classifies on that number,
// so both arms have to produce it.
func TestUnconsentedScopeIsRefused(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	all := s.ConsentedScopes()

	t.Run("authorize", func(t *testing.T) {
		q := authorize(t, s, "st-4", "verifier-eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
			"openid", "offline_access", "499b84ac-1321-427f-aa17-267ca6975798/vso.serviceendpoint_manage")
		if q.Get("error") != entrafake.ErrConsentRequired {
			t.Fatalf("error = %q, want %q", q.Get("error"), entrafake.ErrConsentRequired)
		}
		if !strings.Contains(q.Get("error_description"), entrafake.AADSTSConsentRequired) {
			t.Fatalf("error_description %q does not name %s", q.Get("error_description"), entrafake.AADSTSConsentRequired)
		}
	})

	t.Run("refresh", func(t *testing.T) {
		first := redeemCode(t, s, "verifier-ffffffffffffffffffffffffffffffffffffffff",
			"openid", "offline_access", all[1])
		refresh, _ := first["refresh_token"].(string)
		status, body := postToken(t, s, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {refresh},
			// all[2] was consented to the APP but never to this grant.
			"scope": {all[2]},
		})
		if status == http.StatusOK {
			t.Fatalf("a scope outside this grant was granted: %v", body)
		}
		if got := body["error"]; got != entrafake.ErrConsentRequired {
			t.Fatalf("error = %v, want %q", got, entrafake.ErrConsentRequired)
		}
		if desc, _ := body["error_description"].(string); !strings.Contains(desc, entrafake.AADSTSConsentRequired) {
			t.Fatalf("error_description %q does not name %s", desc, entrafake.AADSTSConsentRequired)
		}
	})
}

// TestRotationInvalidatesTheOldRefreshToken is the reason refresh must be
// control-plane owned: the response carries a NEW refresh token and the
// presented one stops working at once. A party that redeems without persisting
// the rotation holds a dead credential, and this is the shape it dies in.
func TestRotationInvalidatesTheOldRefreshToken(t *testing.T) {
	s := entrafake.New()
	defer s.Close()

	first := redeemCode(t, s, "verifier-0000000000000000000000000000000000000000",
		"openid", "offline_access", s.ConsentedScopes()[1])
	original, _ := first["refresh_token"].(string)

	status, body := postToken(t, s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {original},
	})
	if status != http.StatusOK {
		t.Fatalf("first refresh: status %d body %v", status, body)
	}
	rotated, _ := body["refresh_token"].(string)
	if rotated == "" || rotated == original {
		t.Fatalf("the refresh token did not rotate (%q -> %q)", original, rotated)
	}
	if live, known := s.RefreshTokenState(original); !known || live {
		t.Fatalf("the presented token should be known and RETIRED (live=%v known=%v)", live, known)
	}
	if live, known := s.RefreshTokenState(rotated); !known || !live {
		t.Fatalf("the rotated token should be known and LIVE (live=%v known=%v)", live, known)
	}

	status, body = postToken(t, s, url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {original},
	})
	if status == http.StatusOK {
		t.Fatalf("the retired refresh token was redeemed again: %v", body)
	}
	if got := body["error"]; got != entrafake.ErrInvalidGrant {
		t.Fatalf("error = %v, want %q", got, entrafake.ErrInvalidGrant)
	}
}

// TestInjectedRefusals covers the three knobs a consumer's error
// classification is graded against: a dead grant, a revoked consent and a
// Conditional Access policy that wants a human.
func TestInjectedRefusals(t *testing.T) {
	cases := []struct {
		name  string
		arm   func(*entrafake.Server)
		want  string
		aadst string
	}{
		{"invalid_grant", func(s *entrafake.Server) { s.SetInvalidGrant(true) }, entrafake.ErrInvalidGrant, ""},
		{"consent_required", func(s *entrafake.Server) { s.SetConsentRequired(true) }, entrafake.ErrConsentRequired, entrafake.AADSTSConsentRequired},
		{"interaction_required", func(s *entrafake.Server) { s.SetInteractionRequired(true) }, entrafake.ErrInteractionRequired, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := entrafake.New()
			defer s.Close()
			first := redeemCode(t, s, "verifier-1111111111111111111111111111111111111111",
				"openid", "offline_access", s.ConsentedScopes()[1])
			refresh, _ := first["refresh_token"].(string)

			tc.arm(s)
			status, body := postToken(t, s, url.Values{
				"grant_type":    {"refresh_token"},
				"refresh_token": {refresh},
			})
			if status == http.StatusOK {
				t.Fatalf("the injected refusal did not fire: %v", body)
			}
			if got := body["error"]; got != tc.want {
				t.Fatalf("error = %v, want %q", got, tc.want)
			}
			if tc.aadst != "" {
				if desc, _ := body["error_description"].(string); !strings.Contains(desc, tc.aadst) {
					t.Fatalf("error_description %q does not name %s", desc, tc.aadst)
				}
			}
		})
	}
}

// TestAuthorizeRefusesAnUnknownClientAndRedirectInBand: neither refusal may
// bounce to the redirect_uri the request named, because in exactly the case
// these checks exist for that URI belongs to the attacker.
func TestAuthorizeRefusesAnUnknownClientAndRedirectInBand(t *testing.T) {
	s := entrafake.New()
	defer s.Close()

	base := s.URL() + "/" + s.TenantID() + "/oauth2/v2.0/authorize"
	common := url.Values{
		"response_type":         {"code"},
		"scope":                 {"openid offline_access"},
		"state":                 {"st-5"},
		"code_challenge":        {entrafake.Challenge("verifier-2222222222222222222222222222222222222222")},
		"code_challenge_method": {"S256"},
	}

	t.Run("unknown client", func(t *testing.T) {
		q := url.Values(maps.Clone(common))
		q.Set("client_id", "not-the-app")
		q.Set("redirect_uri", s.RedirectURI())
		assertInBandRefusal(t, base+"?"+q.Encode())
	})
	t.Run("foreign redirect", func(t *testing.T) {
		q := url.Values(maps.Clone(common))
		q.Set("client_id", s.ClientID())
		q.Set("redirect_uri", "https://attacker.example.invalid/callback")
		assertInBandRefusal(t, base+"?"+q.Encode())
	})
}

func assertInBandRefusal(t *testing.T, target string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	resp, err := noRedirectClient().Do(req)
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status %d, want 400 — a refusal like this must not redirect", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "" {
		t.Fatalf("the refusal redirected to %q", loc)
	}
}

// TestAuthorizeRequiresS256 refuses a plain challenge method and a missing
// challenge. `plain` is not a weaker option this fake tolerates: it is not
// offered at all, so a client that fell back to it fails here rather than in
// production.
func TestAuthorizeRequiresS256(t *testing.T) {
	s := entrafake.New()
	defer s.Close()

	base := s.URL() + "/" + s.TenantID() + "/oauth2/v2.0/authorize"
	for _, tc := range []struct{ name, method, challenge string }{
		{"plain", "plain", "verifier-3333333333333333333333333333333333333333"},
		{"absent method", "", "abc"},
		{"absent challenge", "S256", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			q := url.Values{
				"client_id":             {s.ClientID()},
				"redirect_uri":          {s.RedirectURI()},
				"response_type":         {"code"},
				"scope":                 {"openid offline_access"},
				"state":                 {"st-6"},
				"code_challenge":        {tc.challenge},
				"code_challenge_method": {tc.method},
			}
			req, err := http.NewRequest(http.MethodGet, base+"?"+q.Encode(), nil)
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			resp, err := noRedirectClient().Do(req)
			if err != nil {
				t.Fatalf("authorize: %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			loc, err := url.Parse(resp.Header.Get("Location"))
			if err != nil {
				t.Fatalf("parse Location: %v", err)
			}
			if loc.Query().Get("error") != entrafake.ErrInvalidRequest {
				t.Fatalf("error = %q, want %q (status %d)", loc.Query().Get("error"), entrafake.ErrInvalidRequest, resp.StatusCode)
			}
			if loc.Query().Get("code") != "" {
				t.Fatal("a code was issued without an S256 challenge")
			}
		})
	}
}

// TestConfidentialClientRequiresItsSecret: once a secret is configured, /token
// refuses a request that omits or mistypes it.
func TestConfidentialClientRequiresItsSecret(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	s.SetClientSecret("s3cr3t-value-for-the-fake-app")

	verifier := "verifier-4444444444444444444444444444444444444444"
	q := authorize(t, s, "st-7", verifier, "openid", "offline_access")
	status, body := postToken(t, s, url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {q.Get("code")},
		"code_verifier": {verifier},
	})
	if status == http.StatusOK {
		t.Fatalf("a confidential client redeemed with no secret: %v", body)
	}
	if got := body["error"]; got != entrafake.ErrInvalidClient {
		t.Fatalf("error = %v, want %q", got, entrafake.ErrInvalidClient)
	}
}

// TestDeviceCodeGrantIsNotOffered pins the deliberate omission: device code is
// unsupported here because Entra's own Conditional Access baseline recommends
// blocking it and it cannot be bound to a browser session, so a lane that
// reached for it must fail rather than find a fake that obliges.
func TestDeviceCodeGrantIsNotOffered(t *testing.T) {
	s := entrafake.New()
	defer s.Close()
	status, body := postToken(t, s, url.Values{
		"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		"device_code": {"anything"},
	})
	if status == http.StatusOK {
		t.Fatalf("the device code grant was served: %v", body)
	}
	if got := body["error"]; got != entrafake.ErrUnsupportedGrantType {
		t.Fatalf("error = %v, want %q", got, entrafake.ErrUnsupportedGrantType)
	}
}
