// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// overage_role_test.go pins the ROLE half of the Entra claim-overage rule.
//
// 0.7 taught the GROUP SNAPSHOT that an absent `groups`/`roles` claim can mean
// "the IdP declined to send it" rather than "the human is in none of them"
// (sessionGroups' truncation bit, groups_overage_probe_test.go). Role
// derivation was never told. deriveRole is a pure function of the claims it is
// handed, so an overage reaches it as "this human holds no groups and no App
// Roles", its arm-3 fallthrough fires, and WARDYN_OIDC_DEFAULT_ROLE decides the
// session — on `admin`, promoting a member the hidden claim would have walled.
// The trigger is a directory change nobody in Wardyn made or can see.
package oidc_test

import (
	"net/http"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// overageNames is the shape Entra actually sends: `_claim_names` naming the
// claim it OMITTED, pointing at Graph.
func overageNames(claim string) map[string]any {
	return map[string]any{claim: map[string]any{"endpoint": "https://graph.microsoft.com/v1.0/me/getMemberObjects"}}
}

func defaultMatch(role string) []writoidc.Match {
	return []writoidc.Match{{Role: role, Source: writoidc.MatchSourceDefaultRole}}
}

func mapRowMatch(value, role string) []writoidc.Match {
	return []writoidc.Match{{Value: value, Role: role, Source: writoidc.MatchSourceMapRow}}
}

// TestOverageWidensRole is the decision table. The rule has to be narrow in
// three directions at once or it either misses the escalation or breaks
// ordinary logins.
//
// Counterfactual: delete the overageWidensRole call from CallbackHandler, or
// weaken any of its three conjuncts, and a row below goes red.
func TestOverageWidensRole(t *testing.T) {
	cases := []struct {
		name       string
		claimNames map[string]any
		role       string
		matches    []writoidc.Match
		want       bool
	}{
		// THE ESCALATION. The hidden claim is exactly the one that would have
		// walled this human; the default hands them the top tier instead.
		{"groups overage + default admin fallthrough", overageNames("groups"), writoidc.RoleAdmin, defaultMatch(writoidc.RoleAdmin), true},
		// The App Roles half. An overage on `roles` hides a role-mapped tier
		// just as completely — both claims feed the same role map.
		{"roles overage + default admin fallthrough", overageNames("roles"), writoidc.RoleAdmin, defaultMatch(writoidc.RoleAdmin), true},

		// NOT AN ESCALATION, and each of these must keep signing in.
		//
		// A real match is authoritative: hiding a claim can only REMOVE matches
		// from a highest-wins fold, so an overage can only narrow a matched
		// role. Narrowing is the safe direction.
		{"overage but a claim actually matched", overageNames("groups"), writoidc.RoleAdmin, mapRowMatch("wardyn.admins", writoidc.RoleAdmin), false},
		// The ordinary posture: member is the narrowest tier there is, so no
		// hidden claim could have produced less. A human in 200+ groups must
		// still be able to sign in.
		{"overage + default member fallthrough", overageNames("groups"), writoidc.RoleMember, defaultMatch(writoidc.RoleMember), false},
		// No overage: the token answered the question, whatever the answer was.
		{"no claim_names at all", nil, writoidc.RoleAdmin, defaultMatch(writoidc.RoleAdmin), false},
		// A distributed claim this package derives nothing from is not an
		// overage of anything that matters.
		{"an unrelated distributed claim", overageNames("some_other_claim"), writoidc.RoleAdmin, defaultMatch(writoidc.RoleAdmin), false},
		// Arm 1 (no role map): the role comes from the allowlist and the email,
		// never from a claim, and never carries MatchSourceDefaultRole.
		{"arm 1, no matches at all", overageNames("groups"), writoidc.RoleAdmin, nil, false},
		{"arm 1, operator allowlist match", overageNames("groups"), writoidc.RoleAdmin,
			[]writoidc.Match{{Value: "op@corp.example", Role: writoidc.RoleAdmin, Source: writoidc.MatchSourceOperatorAllowlist}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := writoidc.OverageWidensRoleForTest(tc.claimNames, tc.role, tc.matches); got != tc.want {
				t.Fatalf("overageWidensRole = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestOverageLoginDeniedNotPromoted is the end-to-end pin, and it is the one
// that reproduces the finding: ONE human, ONE role map, ONE default, and the
// only difference between the two legs is whether the IdP sent the claim.
//
// Leg 1 (claim present): "walled-contractors" maps to member, so the human is a
// member. Leg 2 (the identical human, in overage): the claim is absent, the
// map matches nothing, and WARDYN_OIDC_DEFAULT_ROLE=admin used to hand them a
// SUPER ADMIN session — full reach into other people's runs, secrets, harness
// credentials and an interactive PTY in any sandbox. It must now be refused
// with a code that says retrying will not help.
//
// Counterfactual: remove the overageWidensRole branch from CallbackHandler and
// leg 2 mints a cookie with Role=admin instead of redirecting.
func TestOverageLoginDeniedNotPromoted(t *testing.T) {
	roleMap := map[string]string{"walled-contractors": writoidc.RoleMember}

	t.Run("claim present: walled to member", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newRoleAuth(t, roleMap, writoidc.RoleAdmin, nil)
		_, sess := doRoleCallback(t, env, auth, "contractor@corp.example", nil, []string{"walled-contractors"})
		if sess.Role != writoidc.RoleMember {
			t.Fatalf("role = %q, want %q — the control leg must be walled, or the overage leg proves nothing", sess.Role, writoidc.RoleMember)
		}
	})

	t.Run("same human in overage: denied, not promoted", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newRoleAuth(t, roleMap, writoidc.RoleAdmin, nil)
		// doCallback, NOT doRoleCallback: the latter re-signs the token and
		// would clobber the overage shape f2BuildOverageIDToken just staged.
		f2BuildOverageIDToken(t, env, "sub-contractor", "contractor@corp.example")
		w, sess := doCallback(t, auth)

		if sess.Role == writoidc.RoleAdmin {
			t.Fatalf("an overage login derived role=admin: the IdP omitted the very claim that walls this human, " +
				"and WARDYN_OIDC_DEFAULT_ROLE promoted them to super admin on the strength of it")
		}
		if w.Code != http.StatusFound {
			t.Fatalf("callback = %d, want 302", w.Code)
		}
		loc := w.Result().Header.Get("Location")
		if !containsAuthError(loc, "claims_overage") {
			t.Fatalf("Location = %q, want auth_error=claims_overage — a denial the operator must act on cannot share no_role's "+
				"\"checked, and nothing matched\" code, because here nothing was checkable", loc)
		}
		// L6: every deny path must actively CLEAR a pre-existing cookie, not
		// merely decline to mint a new one.
		assertSessionCookieCleared(t, w)
	})

	// The narrow rule has to leave the ordinary posture alone: the SAME overage
	// token, with the default set to the narrowest tier, still signs in.
	t.Run("overage + default member still signs in", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newRoleAuth(t, roleMap, writoidc.RoleMember, nil)
		f2BuildOverageIDToken(t, env, "sub-contractor", "contractor@corp.example")
		w, sess := doCallback(t, auth)
		if sess.Role != writoidc.RoleMember {
			t.Fatalf("role = %q (status %d, %q), want %q — a human in 200+ groups must still be able to sign in when the default cannot widen",
				sess.Role, w.Code, w.Result().Header.Get("Location"), writoidc.RoleMember)
		}
	})
}

// TestOverageMarkerCoversBothClaims pins the `roles` arm of the two-claim
// overage rule, which had no test anywhere: the loop reads
// []string{"groups", "roles"} and only the `groups` half was pinned, so
// trimming the loop to one claim left the whole package green while reopening
// the App-Roles overage for governance ceilings and group DENY grants.
//
// Counterfactual: drop "roles" from claimsOverage's loop and the second row
// goes red; drop "groups" and the first does.
func TestOverageMarkerCoversBothClaims(t *testing.T) {
	cases := []struct {
		name       string
		claimNames map[string]any
		want       bool
	}{
		{"groups overage", overageNames("groups"), true},
		{"roles overage (App Roles)", overageNames("roles"), true},
		{"both", map[string]any{"groups": "src1", "roles": "src2"}, true},
		{"an unrelated distributed claim", overageNames("some_other_claim"), false},
		{"no _claim_names", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// A NON-EMPTY claim alongside the marker: the overage bit must not
			// depend on the snapshot happening to come out empty.
			if got := writoidc.SessionGroupsTruncatedForTest(nil, []string{"eng"}, tc.claimNames); got != tc.want {
				t.Fatalf("truncated = %v, want %v for _claim_names %v", got, tc.want, tc.claimNames)
			}
		})
	}
}
