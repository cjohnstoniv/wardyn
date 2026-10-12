// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
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

// TestVerifySubjectTokenEntraPortalToken: on Entra a token issued to the portal
// carries a sub pairwise to the portal's own registration, so the exchange acts
// for the principal the person's own sign-in bound to the token's tenant and
// object id, or for a person set up by that object id. A token whose identity
// has no such principal is refused. A token for Wardyn and every other issuer
// keep their sub.
func TestVerifySubjectTokenEntraPortalToken(t *testing.T) {
	const unbound = "subject_token_unbound"
	portalToken := func(c map[string]any) {
		c["aud"], c["sub"] = testPortalClient, "portal-pairwise"
		delete(c, "azp")
	}
	for _, tc := range []struct {
		name     string
		notEntra bool
		keying   bool
		edit     func(map[string]any)
		gate     *fakeGate
		wantSub  string
		want     string
	}{
		// Unchanged behaviour.
		{name: "a token for Wardyn keeps its sub", edit: func(map[string]any) {},
			gate: &fakeGate{bound: map[string]string{keyObject: "signed-in-sub"}}, wantSub: "person-sub"},
		{name: "another issuer keeps the portal token's sub", notEntra: true, edit: portalToken,
			gate: &fakeGate{bound: map[string]string{keyObject: "signed-in-sub"}}, wantSub: "portal-pairwise"},
		{name: "a person set up by object id who never signed in", keying: true, edit: portalToken,
			gate: &fakeGate{}, wantSub: keyPerson},
		{name: "a person set up by object id who has signed in", keying: true, edit: portalToken,
			gate: &fakeGate{bound: map[string]string{keyObject: keyPerson}}, wantSub: keyPerson},
		{name: "a deactivated identity", edit: portalToken,
			gate: &fakeGate{refused: true, bound: map[string]string{keyObject: "signed-in-sub"}}, want: writoidc.DenialIdentityDeactivated},
		// The principal comes from the identity's binding.
		{name: "the principal the person's sign-in bound", edit: portalToken,
			gate: &fakeGate{bound: map[string]string{keyObject: "signed-in-sub"}}, wantSub: "signed-in-sub"},
		{name: "the binding outranks an object-id person", keying: true, edit: portalToken,
			gate: &fakeGate{bound: map[string]string{keyObject: "signed-in-sub"}}, wantSub: "signed-in-sub"},
		{name: "the bound principal is deactivated", edit: portalToken,
			gate: &fakeGate{deactivated: map[string]bool{"signed-in-sub": true}, bound: map[string]string{keyObject: "signed-in-sub"}},
			want: writoidc.DenialIdentityDeactivated},
		{name: "no sign-in has bound the identity", edit: portalToken, gate: &fakeGate{}, want: unbound},
		{name: "no identity gate", edit: portalToken, want: unbound},
		{name: "no object id", edit: func(c map[string]any) { portalToken(c); delete(c, "oid") },
			gate: &fakeGate{bound: map[string]string{"": "signed-in-sub"}}, want: unbound},
		{name: "no tenant", edit: func(c map[string]any) { portalToken(c); delete(c, "tid") },
			gate: &fakeGate{bound: map[string]string{keyObject: "signed-in-sub"}}, want: unbound},
		{name: "the binding cannot be read", edit: portalToken,
			gate: &fakeGate{boundErr: errors.New("pg down")}, want: "role_check_unavailable"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newRoleMappingAuth(t, nil, writoidc.RoleUser, nil, nil, func(cfg *writoidc.Config) {
				if tc.gate != nil {
					cfg.Identities = tc.gate
				}
			})
			if !tc.notEntra {
				writoidc.SetEntraForTest(auth)
			}
			if tc.keying {
				auth.AttachPersonKeying(&fakeKeying{issuer: env.httpSrv.URL})
			}
			c := env.subjectClaims()
			c["tid"], c["oid"] = keyTenant, keyObject
			tc.edit(c)
			r := httptest.NewRequest("POST", "/api/v1/token", nil)
			sess, denied := auth.VerifySubjectToken(r, env.sign(t, c), testPortalClient, nil)
			if denied != tc.want || sess.Sub != tc.wantSub {
				t.Fatalf("exchange = (%q, %q), want (%q, %q)", sess.Sub, denied, tc.wantSub, tc.want)
			}
		})
	}
}
