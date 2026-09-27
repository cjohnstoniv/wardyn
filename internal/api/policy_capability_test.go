// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestCapabilityPolicyKind: `policy` narrows the stored policy a person
// SELECTS (policy_id), on launch and preflight alike, and nothing else. Every
// subtest asks both doors the same question, because a Review that previews a
// clean checklist for a run launch then refuses is the drift the parity guard
// exists for.
func TestCapabilityPolicyKind(t *testing.T) {
	allow, deny := types.CapabilityAllow, types.CapabilityDeny
	stored := types.RunPolicy{ID: uuid.New(), Name: "research", Spec: govDeployment()}
	enforced := func() map[string]bool { return map[string]bool{capPolicy: true} }
	withPolicy := func(id uuid.UUID) string {
		return `{"agent":"claude-code","task":"t","policy_id":"` + id.String() + `"}`
	}
	person := func(t *testing.T) *http.Cookie { return govSession(t, govMemberSub, []string{"eng"}, false) }

	// both POSTs body to each door on a fresh fixture and returns the two codes
	// plus the authz.denied reasons the pair wrote.
	both := func(t *testing.T, cs *capStore, cookie *http.Cookie, body string) (create, preflight int, reasons []string, msg string) {
		t.Helper()
		cs.userTypes = utKnown
		srv, st, _ := govEscapeFixture(t, cs)
		st.policies[stored.ID] = stored
		w := doSSO(t, srv, http.MethodPost, "/api/v1/runs/preflight", cookie, body)
		preflight = w.Code
		msg = w.Body.String()
		create = doSSO(t, srv, http.MethodPost, "/api/v1/runs", cookie, body).Code
		return create, preflight, auditReasons(t, srv, "authz.denied"), msg
	}
	wantRefused := func(t *testing.T, create, preflight int, reasons []string, msg string) {
		t.Helper()
		if create != http.StatusForbidden || preflight != http.StatusForbidden {
			t.Fatalf("create = %d, preflight = %d, want 403 on both doors: %s", create, preflight, msg)
		}
		if n := strings.Count(strings.Join(reasons, ","), "capability_policy"); n != 2 {
			t.Errorf("authz.denied reasons = %v, want capability_policy from each door", reasons)
		}
		if !strings.Contains(msg, "Stored policy ") || !strings.Contains(msg, "Ask your admin") {
			t.Errorf("body = %s, want the capitalised refusal naming the policy", msg)
		}
	}
	wantAllowed := func(t *testing.T, create, preflight int, reasons []string, msg string) {
		t.Helper()
		if create != http.StatusCreated || preflight != http.StatusOK {
			t.Fatalf("create = %d, preflight = %d, want 201 / 200: %s", create, preflight, msg)
		}
		if slices.Contains(reasons, "capability_policy") {
			t.Errorf("authz.denied reasons = %v, want no capability_policy", reasons)
		}
	}

	t.Run("unenforced, no grant: allowed (upgrade day is unchanged)", func(t *testing.T) {
		c, p, r, m := both(t, &capStore{}, person(t), withPolicy(stored.ID))
		wantAllowed(t, c, p, r, m)
	})

	t.Run("enforced, no grant: refused on both doors", func(t *testing.T) {
		c, p, r, m := both(t, &capStore{enf: enforced()}, person(t), withPolicy(stored.ID))
		wantRefused(t, c, p, r, m)
	})

	t.Run("enforced, no grant, an id that names no row: 403, not a 400 that says it is missing", func(t *testing.T) {
		c, p, r, m := both(t, &capStore{enf: enforced()}, person(t), withPolicy(uuid.New()))
		wantRefused(t, c, p, r, m)
	})

	t.Run("enforced, no grant, no policy_id: allowed (the caller's own ceiling is not a choice)", func(t *testing.T) {
		c, p, r, m := both(t, &capStore{enf: enforced()}, person(t), `{"agent":"claude-code","task":"t"}`)
		wantAllowed(t, c, p, r, m)
	})

	t.Run("enforced, an allow on the id: allowed", func(t *testing.T) {
		c, p, r, m := both(t, &capStore{enf: enforced(), grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectUser, govMemberSub, capPolicy, stored.ID.String(), allow),
		}}, person(t), withPolicy(stored.ID))
		wantAllowed(t, c, p, r, m)
	})

	t.Run("enforced, an allow on the caller's user type: allowed; another type's: refused", func(t *testing.T) {
		rows := func() []types.CapabilityGrant {
			return []types.CapabilityGrant{grant(types.CapabilitySubjectUserType, utPM, capPolicy, stored.ID.String(), allow)}
		}
		pm := ssoSessionOfType(t, govMemberSub, "pm@corp.example", oidc.RoleUser, utPM)
		c, p, r, m := both(t, &capStore{enf: enforced(), grants: rows()}, pm, withPolicy(stored.ID))
		wantAllowed(t, c, p, r, m)
		dev := ssoSessionOfType(t, govMemberSub, "dev@corp.example", oidc.RoleUser, utDev)
		c, p, r, m = both(t, &capStore{enf: enforced(), grants: rows()}, dev, withPolicy(stored.ID))
		wantRefused(t, c, p, r, m)
	})

	t.Run("unenforced, an `all` deny on the id: refused (a deny bites before the switch)", func(t *testing.T) {
		c, p, r, m := both(t, &capStore{grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capPolicy, stored.ID.String(), deny),
		}}, person(t), withPolicy(stored.ID))
		wantRefused(t, c, p, r, m)
	})

	t.Run("a security admin is bounded like anyone", func(t *testing.T) {
		sec := ssoSession(t, secAdminSub, secAdminMail, oidc.RoleSecurityAdmin)
		c, p, r, m := both(t, &capStore{enf: enforced()}, sec, withPolicy(stored.ID))
		wantRefused(t, c, p, r, m)
	})

	t.Run("a super admin is exempt", func(t *testing.T) {
		admin := ssoSession(t, "sub-admin", "admin@corp.example", oidc.RoleAdmin)
		c, p, r, m := both(t, &capStore{enf: enforced(), grants: []types.CapabilityGrant{
			grant(types.CapabilitySubjectAll, "", capPolicy, capWildcard, deny),
		}}, admin, withPolicy(stored.ID))
		wantAllowed(t, c, p, r, m)
	})
}

// TestPolicyGrantValueIsACanonicalUUID: the resolver compares a policy value
// against uuid.UUID.String() exactly, so the write boundary folds every
// spelling uuid.Parse reads onto that one and refuses anything else.
func TestPolicyGrantValueIsACanonicalUUID(t *testing.T) {
	id := uuid.New()
	got, err := canonicalGrantValue(capPolicy, "{"+strings.ToUpper(id.String())+"}")
	if err != nil || got != id.String() {
		t.Fatalf("canonicalGrantValue(braced upper) = %q, %v, want %q", got, err, id.String())
	}
	if _, err := canonicalGrantValue(capPolicy, "research"); err == nil {
		t.Fatal("a policy NAME was accepted as a value; it can never match the uuid the resolver compares")
	}
	if got, err := canonicalGrantValue(capPolicy, capWildcard); err != nil || got != capWildcard {
		t.Fatalf("wildcard = %q, %v", got, err)
	}
}
