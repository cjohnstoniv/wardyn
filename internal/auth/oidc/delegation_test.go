// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

const testPortalClient = "portal-client"

// subjectClaims is a person's token as the test IdP issues it; each case edits
// a copy.
func (e *idpEnv) subjectClaims() map[string]any {
	now := time.Now()
	return map[string]any{
		"iss": e.httpSrv.URL, "sub": "person-sub", "aud": e.clientID, "azp": testPortalClient,
		"email": "person@corp.example", "email_verified": true, "groups": []string{"Portal-Users"},
		"iat": now.Unix(), "exp": now.Add(5 * time.Minute).Unix(),
	}
}

func (e *idpEnv) sign(t *testing.T, claims map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return oidctest.SignIDToken(e.priv, "test-key", "RS256", string(raw))
}

// TestVerifySubjectToken is the subject half of a portal's exchange (#1142):
// only a token that verifies against this deployment's issuer and key set, and
// was issued to the portal or for Wardyn at the portal's request, admits its
// subject — through the same admission a sign-in makes.
func TestVerifySubjectToken(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newAuth(t, []string{"corp.example"})
	otherKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	edit := func(f func(map[string]any)) string {
		c := env.subjectClaims()
		f(c)
		return env.sign(t, c)
	}
	unsignedNone := func() string {
		hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
		body, _ := json.Marshal(env.subjectClaims())
		return hdr + "." + base64.RawURLEncoding.EncodeToString(body) + "."
	}
	for _, tc := range []struct {
		name   string
		token  string
		portal string
		want   string
	}{
		{"access token for Wardyn requested by the portal", edit(func(map[string]any) {}), testPortalClient, ""},
		{"the portal's own ID token", edit(func(c map[string]any) { c["aud"] = testPortalClient; delete(c, "azp") }), testPortalClient, ""},
		{"the portal's own ID token, azp agreeing", edit(func(c map[string]any) { c["aud"] = testPortalClient }), testPortalClient, ""},
		{"for Wardyn but requested by another client", edit(func(c map[string]any) { c["azp"] = "other-client" }), testPortalClient, writoidc.SubjectTokenAudience},
		{"for Wardyn with no azp", edit(func(c map[string]any) { delete(c, "azp") }), testPortalClient, writoidc.SubjectTokenAudience},
		{"issued to another client", edit(func(c map[string]any) { c["aud"] = "other-client"; c["azp"] = "other-client" }), testPortalClient, writoidc.SubjectTokenAudience},
		{"the portal's ID token naming another azp", edit(func(c map[string]any) { c["aud"] = testPortalClient; c["azp"] = "other-client" }), testPortalClient, writoidc.SubjectTokenAudience},
		{"two audiences without Wardyn", edit(func(c map[string]any) { c["aud"] = []string{testPortalClient, "other-client"}; delete(c, "azp") }), testPortalClient, writoidc.SubjectTokenAudience},
		{"a portal registered under Wardyn's own client id", edit(func(c map[string]any) { c["azp"] = env.clientID }), env.clientID, writoidc.SubjectTokenAudience},
		{"expired", edit(func(c map[string]any) { c["exp"] = time.Now().Add(-time.Minute).Unix() }), testPortalClient, writoidc.SubjectTokenInvalid},
		{"no exp", edit(func(c map[string]any) { delete(c, "exp") }), testPortalClient, writoidc.SubjectTokenInvalid},
		{"wrong issuer", edit(func(c map[string]any) { c["iss"] = "https://idp.invalid" }), testPortalClient, writoidc.SubjectTokenInvalid},
		{"alg=none", unsignedNone(), testPortalClient, writoidc.SubjectTokenInvalid},
		{"signed by another key", func() string {
			raw, _ := json.Marshal(env.subjectClaims())
			return oidctest.SignIDToken(otherKey, "test-key", "RS256", string(raw))
		}(), testPortalClient, writoidc.SubjectTokenInvalid},
		{"not a JWT", "wdg_" + strings.Repeat("a", 64), testPortalClient, writoidc.SubjectTokenInvalid},
		{"no iat", edit(func(c map[string]any) { delete(c, "iat") }), testPortalClient, writoidc.SubjectTokenNoIssuedAt},
		{"reserved subject", edit(func(c map[string]any) { c["sub"] = "admin-token" }), testPortalClient, "sign_in_refused"},
		{"email outside the allowed domains", edit(func(c map[string]any) { c["email"] = "person@elsewhere.example" }), testPortalClient, "email_domain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/api/v1/token", nil)
			sess, denied := auth.VerifySubjectToken(r, tc.token, tc.portal, func(sub string) bool { return sub == "admin-token" })
			if denied != tc.want {
				t.Fatalf("denied = %q, want %q", denied, tc.want)
			}
			if tc.want != "" {
				if sess.Sub != "" {
					t.Fatalf("a refusal returned an identity: %+v", sess)
				}
				return
			}
			if sess.Sub != "person-sub" || sess.Email != "person@corp.example" || sess.Role == "" || sess.UserType == "" ||
				len(sess.Groups) != 1 || sess.Groups[0] != "portal-users" || sess.IssuedAt.IsZero() || sess.Expiry.IsZero() {
				t.Fatalf("admitted session = %+v, want the person with a derived role, type and canonical groups", sess)
			}
		})
	}
}
