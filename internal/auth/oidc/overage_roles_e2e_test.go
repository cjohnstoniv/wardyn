// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// overage_roles_e2e_test.go closes the one arm of the claim-overage rule that
// had no END-TO-END pin: the App Roles half.
//
// claimsOverage reads []string{"groups", "roles"} and both arms are pinned as
// units (TestOverageWidensRole's two overage rows, TestOverageMarkerCoversBothClaims).
// Everything ABOVE that unit — CallbackHandler's `_claim_names` decode, the
// denial branch, the cleared cookie, the auth_error code — was only ever driven
// with a `groups` overage (f2BuildOverageIDToken omits `groups` and nothing
// else). So the whole path from a signed token to a refused login was pinned on
// one claim of two, and an Entra tenant that overflows APP ROLES rather than
// groups took the untested road.
//
// The distinction is not hypothetical: `roles` is the claim Wardyn's own
// documentation recommends as the REMEDY for a groups overage (carry the tier
// on App Roles, the much smaller claim), so it is exactly the claim a
// groups-overflowing tenant is told to move onto — and App Role assignments
// overflow too.
package oidc_test

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc/oidctest"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// buildClaimOverageIDToken signs the full Entra overage shape for ONE claim:
// that claim absent, BOTH halves of the distributed-claim pointer present, and
// the OTHER derivation claim sent normally — the real tenant shape, where an
// overage hides one claim at a time rather than emptying the token.
//
// Both pointer halves are required: go-oidc's verifier rejects a `_claim_names`
// entry with no matching `_claim_sources` source outright, which would 401 the
// login before any Wardyn code ran and make this pin pass for the wrong reason.
func buildClaimOverageIDToken(t *testing.T, e *idpEnv, sub, email, overClaim string, otherClaim string, otherValue []string) {
	t.Helper()
	claims := map[string]any{
		"iss": e.httpSrv.URL, "sub": sub, "aud": e.clientID,
		"email": email, "email_verified": true, "nonce": roleCallbackNonce,
		"iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix(),
		// overClaim deliberately ABSENT — that is the overage.
		"_claim_names": map[string]any{overClaim: "src1"},
		"_claim_sources": map[string]any{"src1": map[string]any{
			"endpoint": "https://graph.microsoft.com/v1.0/users/" + sub + "/getMemberObjects",
		}},
	}
	if otherClaim != "" {
		claims[otherClaim] = otherValue
	}
	raw, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal overage claims: %v", err)
	}
	e.latestIDTok = oidctest.SignIDToken(e.priv, "test-key", "RS256", string(raw))
}

// TestClaimOverageDeniesTheWideningDefaultForBothClaims drives the real
// CallbackHandler once per claim. One human, one role map, one default; the
// only difference between the legs is WHICH claim the IdP declined to send.
//
// Counterfactual (run it): drop "roles" from claimsOverage's loop and the
// second leg mints a cookie with Role=admin — a super admin session, full reach
// into other people's runs, credentials and sandboxes, handed out because an
// App Role assignment count crossed a limit in the directory.
func TestClaimOverageDeniesTheWideningDefaultForBothClaims(t *testing.T) {
	roleMap := map[string]string{"walled-contractors": writoidc.RoleMember}

	cases := []struct {
		name       string
		overClaim  string
		otherClaim string
		otherValue []string
	}{
		{"groups overage, roles sent normally", "groups", "roles", []string{"Some.Unmapped.Role"}},
		{"roles overage, groups sent normally", "roles", "groups", []string{"some-unmapped-group"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newRoleAuth(t, roleMap, writoidc.RoleAdmin, nil)
			buildClaimOverageIDToken(t, env, "sub-"+tc.overClaim, "contractor@corp.example", tc.overClaim, tc.otherClaim, tc.otherValue)
			w, sess := doCallback(t, auth)

			if sess.Role == writoidc.RoleAdmin {
				t.Fatalf("a %q overage login derived role=admin: the IdP declined to send the claim the role map is keyed on, "+
					"and WARDYN_OIDC_DEFAULT_ROLE promoted this human to super admin on the strength of it", tc.overClaim)
			}
			if w.Code != http.StatusFound {
				t.Fatalf("callback = %d, want 302", w.Code)
			}
			if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, "claims_overage") {
				t.Fatalf("Location = %q, want auth_error=claims_overage — nothing was checkable here, so this cannot share no_role's "+
					"\"checked, and nothing matched\" code, and retrying sends the same token", loc)
			}
			assertSessionCookieCleared(t, w)
		})
	}

	// The narrow rule leaves the ordinary posture alone on BOTH claims: a
	// tenant that overflows either one still signs its people in when the
	// default cannot widen, because member is the narrowest tier there is.
	for _, overClaim := range []string{"groups", "roles"} {
		t.Run(overClaim+" overage + default member still signs in", func(t *testing.T) {
			env := newIdPEnv(t)
			auth := env.newRoleAuth(t, roleMap, writoidc.RoleMember, nil)
			buildClaimOverageIDToken(t, env, "sub-ok-"+overClaim, "contractor@corp.example", overClaim, "", nil)
			w, sess := doCallback(t, auth)
			if sess.Role != writoidc.RoleMember {
				t.Fatalf("role = %q (status %d, %q), want %q — a human in 200+ groups or App Roles must still be able to sign in",
					sess.Role, w.Code, w.Result().Header.Get("Location"), writoidc.RoleMember)
			}
		})
	}
}
