// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// unreadable_claim_test.go pins the THIRD way a login's group/role claims can
// be unanswerable, alongside the byte cap and the Entra overage: the IdP DID
// send the claim, in a shape this build cannot decode.
//
// CallbackHandler decodes `roles` and `groups` tolerantly, one struct each, so
// a scalar string ("eng-team" instead of ["eng-team"]) cannot fail the whole
// login — a real IdP sends that shape and folding it into the fatal claims
// struct would be a 100% login outage (TestCallbackScalarClaim*). But the
// decode ERROR was discarded, so the claim came out nil and the snapshot read
// COMPLETE-and-empty: byte-for-byte "asked, there were none". A group the
// human really holds vanished with truncated=false, which is exactly the
// evaporation the PF-26 bit exists to prevent — capScan never consults
// capUnresolvableGroupDeny, effectiveCeiling never takes
// ceilingWithUnusableGroups, and a group-subject DENY protects nothing.
//
// Tolerating the shape and REPORTING the loss are different jobs. The claim
// still contributes nothing to derivation (coercing a scalar into a
// one-element group would let the raw string match a role-map row —
// TestCallbackScalarGroupsClaimMapSetContributesNothing pins that it must
// not), and the snapshot now says so.
package oidc_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// unreadableSnapshot drives one login carrying `claim` with the given raw JSON
// shape and returns the group snapshot the session actually carries, plus its
// PF-26 partial bit. The bit is only readable off a decoded cookie, which is
// the point: it has to survive encodeSession to be an authorization input.
func unreadableSnapshot(t *testing.T, claim string, val any) (groups []string, truncated bool, ok bool) {
	t.Helper()
	env := newIdPEnv(t)
	// Role map unset: arm 1 of deriveRole, so every leg below signs in and the
	// only thing under test is what the SNAPSHOT reports.
	auth := env.newRoleAuth(t, nil, "", nil)
	env.buildIDTokenRawClaim(t, "sub-unreadable", "unreadable@corp.example", claim, val)
	w, _ := doCallback(t, auth)

	var sessCookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "wardyn_session" && c.MaxAge != -1 {
			sessCookie = c
		}
	}
	if sessCookie == nil {
		return nil, false, false
	}
	next := http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		groups = writoidc.GroupsFromContext(r.Context())
		truncated = writoidc.GroupsTruncatedFromContext(r.Context())
	})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.AddCookie(sessCookie)
	auth.Middleware(next).ServeHTTP(httptest.NewRecorder(), req)
	return groups, truncated, true
}

// TestUnreadableClaimMarksSnapshotPartial is the group half.
//
// Counterfactual: discard the decode errors in CallbackHandler again (`_ =
// idToken.Claims(&gc)`) and every want-true row below reports a COMPLETE empty
// snapshot while the human's real groups are missing from it.
func TestUnreadableClaimMarksSnapshotPartial(t *testing.T) {
	cases := []struct {
		name  string
		claim string
		val   any
		want  bool
	}{
		// The shape a real IdP sends: one group as a bare string.
		{"scalar groups claim", "groups", "eng-team", true},
		// The App Roles half — both claims feed the same snapshot.
		{"scalar roles claim", "roles", "Wardyn.Contractors", true},
		{"object groups claim", "groups", map[string]any{"value": "eng-team"}, true},
		{"array of non-strings", "groups", []any{1, 2}, true},
		// The bit must NOT fire on an ordinary login, or every caller is
		// refused with groups_snapshot_stale forever and PF-26 means nothing.
		{"a well-formed groups array", "groups", []any{"eng-team"}, false},
		{"an absent claim is answered, not unreadable", "some_other_claim", "x", false},
		{"a null groups claim", "groups", nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups, truncated, ok := unreadableSnapshot(t, tc.claim, tc.val)
			if !ok {
				t.Fatalf("no session cookie issued — the tolerant decode must never fail a login (claim %q = %v)", tc.claim, tc.val)
			}
			if truncated != tc.want {
				t.Fatalf("truncated = %v, want %v for claim %q = %v (snapshot %+q)", truncated, tc.want, tc.claim, tc.val, groups)
			}
		})
	}
}

// TestUnreadableClaimDoesNotWidenTheDefaultRole is the ROLE half, and it is the
// same fail-closed choice the Entra overage already makes: deriveRole's arm-3
// fallthrough to WARDYN_OIDC_DEFAULT_ROLE is "nothing matched", and on an
// unreadable claim that is an absence of evidence, not a fact — the very claim
// the role map is keyed on is the one nobody could read.
//
// Counterfactual: drop the unanswerableWidensRole branch from CallbackHandler
// and leg 2 mints a cookie with Role=admin.
func TestUnreadableClaimDoesNotWidenTheDefaultRole(t *testing.T) {
	roleMap := map[string]string{"walled-contractors": writoidc.RoleMember}

	t.Run("claim readable: walled to member", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newRoleAuth(t, roleMap, writoidc.RoleAdmin, nil)
		_, sess := doRoleCallback(t, env, auth, "contractor@corp.example", nil, []string{"walled-contractors"})
		if sess.Role != writoidc.RoleMember {
			t.Fatalf("role = %q, want %q — the control leg must be walled, or the unreadable leg proves nothing", sess.Role, writoidc.RoleMember)
		}
	})

	t.Run("same human, unreadable claim: denied, not promoted", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newRoleAuth(t, roleMap, writoidc.RoleAdmin, nil)
		env.buildIDTokenRawClaim(t, "sub-contractor", "contractor@corp.example", "groups", "walled-contractors")
		w, sess := doCallback(t, auth)
		if sess.Role == writoidc.RoleAdmin {
			t.Fatalf("an unreadable-claim login derived role=admin: the claim that walls this human could not be read, " +
				"and WARDYN_OIDC_DEFAULT_ROLE promoted them to super admin on the strength of it")
		}
		if w.Code != http.StatusFound {
			t.Fatalf("callback = %d, want 302", w.Code)
		}
		if loc := w.Result().Header.Get("Location"); !containsAuthError(loc, "claims_overage") {
			t.Fatalf("Location = %q, want auth_error=claims_overage — retrying sends the same token, so this cannot share no_role's code", loc)
		}
		assertSessionCookieCleared(t, w)
	})

	// The narrow rule leaves the ordinary posture alone: the same token with
	// the default set to the narrowest tier still signs in, because no hidden
	// claim could have produced less than member.
	t.Run("unreadable claim + default member still signs in", func(t *testing.T) {
		env := newIdPEnv(t)
		auth := env.newRoleAuth(t, roleMap, writoidc.RoleMember, nil)
		env.buildIDTokenRawClaim(t, "sub-contractor2", "contractor2@corp.example", "groups", "walled-contractors")
		w, sess := doCallback(t, auth)
		if sess.Role != writoidc.RoleMember {
			t.Fatalf("role = %q (status %d, %q), want %q — a malformed claim must not lock out a deployment whose default cannot widen",
				sess.Role, w.Code, w.Result().Header.Get("Location"), writoidc.RoleMember)
		}
	})
}

// TestUnreadableClaimNamesIsRefusedUpstream records WHERE the third unreadable
// claim is answered, so the fail-closed reasoning above is not assumed to cover
// it. `_claim_names` is the marker that tells an OMITTED claim from an empty
// one, and a marker that cannot be read is a question that cannot be answered —
// but CallbackHandler never sees such a token: go-oidc parses the distributed
// -claim block during Verify and refuses the id_token outright, which is the
// same fail-closed answer one step earlier (401, no session, no cookie).
//
// Pinned because the alternative is silent: if go-oidc ever became tolerant
// here, an unreadable marker would reach the handler as "no overage" and the
// snapshot would read complete.
func TestUnreadableClaimNamesIsRefusedUpstream(t *testing.T) {
	if _, _, ok := unreadableSnapshot(t, "_claim_names", "not-an-object"); ok {
		t.Fatal("an id_token whose _claim_names cannot be parsed produced a SESSION — " +
			"the overage marker is unreadable, so CallbackHandler must decide it rather than assume no overage")
	}
}
