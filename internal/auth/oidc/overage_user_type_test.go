// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// overage_user_type_test.go pins the USER-TYPE half of the claim-overage rule:
// a `groups` or `roles` claim the token did not answer (an Entra overage, or a
// shape this build cannot decode) refuses the sign-in when a role-map row keyed
// on such a claim could have changed the user type the session is stamped
// with, and leaves the sign-in alone when the claims the token did answer
// already settle the type.
package oidc_test

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// overageTypes: three custom types at distinct priorities.
var overageTypes = []types.UserType{
	{ID: types.UserTypeStandard, Name: "Standard user", BuiltIn: true},
	{ID: "lead", Name: "Lead", Priority: 30},
	{ID: "contractor", Name: "Contractor", Priority: 20},
	{ID: "developer", Name: "Developer", Priority: 5},
}

// overageTypeCase is one sign-in: the role map and default, the claim the
// token withholds (overClaim, "" for none), and the claims it does send.
type overageTypeCase struct {
	name        string
	roleMap     map[string]string
	defaultRole string
	overClaim   string // "groups" or "roles"; "" sends both claims normally
	unreadable  any    // non-nil: send `groups` as this undecodable value instead
	roles       []string
	groups      []string
	email       string
}

// signInWithTypes drives the real CallbackHandler for one case and returns the
// response and the session it minted (zero when refused).
func signInWithTypes(t *testing.T, tc overageTypeCase) (*http.Response, writoidc.Session) {
	t.Helper()
	env := newIdPEnv(t)
	auth := env.newRoleMappingAuth(t, tc.roleMap, tc.defaultRole, nil, nil,
		func(c *writoidc.Config) { c.UserTypes = &fakeUserTypeSource{list: overageTypes} })
	email := tc.email
	if email == "" {
		email = "pat@corp.example"
	}
	switch {
	case tc.unreadable != nil:
		env.buildIDTokenRawClaim(t, "sub-pat", email, "groups", tc.unreadable)
	case tc.overClaim != "":
		// The other claim is sent only when the case names values for it.
		other, values := "roles", tc.roles
		if tc.overClaim == "roles" {
			other, values = "groups", tc.groups
		}
		if values == nil {
			other = ""
		}
		buildClaimOverageIDToken(t, env, "sub-pat", email, tc.overClaim, other, values)
	default:
		env.buildIDTokenWithRoles(t, "sub-pat", email, tc.roles, tc.groups, roleCallbackNonce, time.Now().Add(time.Hour))
	}
	w, sess := doCallback(t, auth)
	return w.Result(), sess
}

// TestUnansweredClaimKeepsSettledUserType: sign-ins whose user type the
// answered claims settle keep signing in on that type, with a claim withheld.
func TestUnansweredClaimKeepsSettledUserType(t *testing.T) {
	cases := []struct {
		overageTypeCase
		role, userType string
	}{
		// Control: both claims sent, the group names the type.
		{overageTypeCase{name: "no overage, group names the type", roleMap: map[string]string{"contractors-gid": "contractor"},
			defaultRole: writoidc.RoleUser, groups: []string{"contractors-gid"}}, writoidc.RoleUser, "contractor"},
		// No row names a custom type: nothing hidden could change "standard".
		{overageTypeCase{name: "groups overage, only tier rows", roleMap: map[string]string{"staff-gid": writoidc.RoleUser, "ops-gid": writoidc.RoleSecurityAdmin},
			defaultRole: writoidc.RoleUser, overClaim: "groups"}, writoidc.RoleUser, types.UserTypeStandard},
		// The App Role names the highest-priority type any row names.
		{overageTypeCase{name: "groups overage, answered claim names the top type", roleMap: map[string]string{"contractors-gid": "contractor", "app-lead": "lead"},
			defaultRole: writoidc.RoleUser, overClaim: "groups", roles: []string{"app-lead"}}, writoidc.RoleUser, "lead"},
		// A type is irrelevant on the admin tier.
		{overageTypeCase{name: "groups overage, answered claim derives admin", roleMap: map[string]string{"contractors-gid": "contractor", "app-admin": writoidc.RoleAdmin},
			defaultRole: writoidc.RoleUser, overClaim: "groups", roles: []string{"app-admin"}}, writoidc.RoleAdmin, types.UserTypeStandard},
		// Control: an email-shaped key is matched by a group value like any other.
		{overageTypeCase{name: "no overage, an email-shaped group names the type", roleMap: map[string]string{"contractors@corp.example": "contractor"},
			defaultRole: writoidc.RoleUser, groups: []string{"contractors@corp.example"}}, writoidc.RoleUser, "contractor"},
		// The row keyed on the caller's own email is answered by the email claim.
		{overageTypeCase{name: "groups overage, own email row names the top type", roleMap: map[string]string{"pat@corp.example": "lead", "contractors-gid": "contractor"},
			defaultRole: writoidc.RoleUser, overClaim: "groups"}, writoidc.RoleUser, "lead"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, sess := signInWithTypes(t, tc.overageTypeCase)
			if sess.Role != tc.role || sess.UserType != tc.userType {
				t.Fatalf("session = role %q type %q (status %d, %q), want %q on %q",
					sess.Role, sess.UserType, resp.StatusCode, resp.Header.Get("Location"), tc.role, tc.userType)
			}
		})
	}
}

// TestUnansweredClaimRefusesWhenItCouldChangeUserType: a withheld or
// undecodable claim that a type-naming row is keyed on refuses the sign-in
// with claims_overage instead of stamping the type the answered claims alone
// derive.
func TestUnansweredClaimRefusesWhenItCouldChangeUserType(t *testing.T) {
	cases := []overageTypeCase{
		// Standard fallback: nothing answered matched, so the default's type
		// would stand in for the one the withheld group names.
		{name: "groups overage, default user", roleMap: map[string]string{"contractors-gid": "contractor"},
			defaultRole: writoidc.RoleUser, overClaim: "groups"},
		{name: "groups overage, default names a type", roleMap: map[string]string{"contractors-gid": "contractor"},
			defaultRole: "developer", overClaim: "groups"},
		{name: "roles overage, default user", roleMap: map[string]string{"app-contractor": "contractor"},
			defaultRole: writoidc.RoleUser, overClaim: "roles"},
		{name: "unreadable groups claim, default user", roleMap: map[string]string{"contractors-gid": "contractor"},
			defaultRole: writoidc.RoleUser, unreadable: "contractors-gid"},
		// Competing match: an answered claim names a lower-priority type that
		// the withheld claim's type would outrank.
		{name: "groups overage, answered App Role names a lower type", roleMap: map[string]string{"contractors-gid": "contractor", "app-dev": "developer"},
			defaultRole: writoidc.RoleUser, overClaim: "groups", roles: []string{"app-dev"}},
		{name: "roles overage, answered group names a lower type", roleMap: map[string]string{"app-contractor": "contractor", "dev-gid": "developer"},
			defaultRole: writoidc.RoleUser, overClaim: "roles", groups: []string{"dev-gid"}},
		// An email-shaped key is not answered by the email claim unless it is the
		// caller's own: a withheld group value can be that address.
		{name: "groups overage, an email-shaped group row names the type", roleMap: map[string]string{"contractors@corp.example": "contractor"},
			defaultRole: writoidc.RoleUser, overClaim: "groups"},
		{name: "groups overage, another person's email row names a higher type", roleMap: map[string]string{"other@corp.example": "lead", "app-dev": "developer"},
			defaultRole: writoidc.RoleUser, overClaim: "groups", roles: []string{"app-dev"}},
		{name: "groups overage, answered email row names a lower type", roleMap: map[string]string{"pat@corp.example": "developer", "contractors-gid": "contractor"},
			defaultRole: writoidc.RoleUser, overClaim: "groups"},
		{name: "groups overage, security admin on a lower type", roleMap: map[string]string{"contractors-gid": "contractor", "app-ops": writoidc.RoleSecurityAdmin, "app-dev": "developer"},
			defaultRole: writoidc.RoleUser, overClaim: "groups", roles: []string{"app-ops", "app-dev"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, sess := signInWithTypes(t, tc)
			if sess.Role != "" {
				t.Fatalf("a session was issued (role %q, type %q) although a claim the token did not answer is keyed to a user type that would change it",
					sess.Role, sess.UserType)
			}
			if resp.StatusCode != http.StatusFound || !containsAuthError(resp.Header.Get("Location"), "claims_overage") {
				t.Fatalf("callback = %d %q, want 302 with auth_error=claims_overage", resp.StatusCode, resp.Header.Get("Location"))
			}
		})
	}
}

// TestUnansweredClaimRefusesThePortalExchange: a portal's subject token goes
// through the same admission as a sign-in, so a withheld groups claim that a
// type-naming row could answer refuses the exchange too.
func TestUnansweredClaimRefusesThePortalExchange(t *testing.T) {
	env := newIdPEnv(t)
	auth := env.newRoleMappingAuth(t, map[string]string{"contractors@corp.example": "contractor"}, writoidc.RoleUser, nil, nil,
		func(c *writoidc.Config) { c.UserTypes = &fakeUserTypeSource{list: overageTypes} })
	claims := env.subjectClaims()
	delete(claims, "groups")
	claims["_claim_names"] = map[string]any{"groups": "src1"}
	claims["_claim_sources"] = map[string]any{"src1": map[string]any{
		"endpoint": "https://graph.microsoft.com/v1.0/users/person-sub/getMemberObjects",
	}}
	r := httptest.NewRequest(http.MethodPost, "/api/v1/token", nil)
	sess, denied := auth.VerifySubjectToken(r, env.sign(t, claims), testPortalClient, func(string) bool { return false })
	if denied != "claims_overage" || sess.Sub != "" {
		t.Fatalf("exchange = %+v denied %q, want a refusal with claims_overage", sess, denied)
	}
}
