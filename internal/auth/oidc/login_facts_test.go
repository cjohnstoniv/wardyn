// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	writoidc "github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

// TestCallbackLoginFacts pins what OnLogin is told about who signed in: the verified issuer and
// email on every issuer, and the tenant and object id only for an Entra token that carries both,
// the object id well-formed.
func TestCallbackLoginFacts(t *testing.T) {
	for _, c := range []struct {
		name              string
		entra             bool
		oid               string
		wantTenant, wantO string
	}{
		{name: "entra with an object id", entra: true, oid: keyObject, wantTenant: keyTenant, wantO: keyObject},
		{name: "entra with a malformed object id", entra: true, oid: "not-a-guid"},
		{name: "entra with no object id", entra: true},
		{name: "other issuer ignores tid and oid", oid: keyObject},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := newIdPEnv(t)
			var got []writoidc.LoginFacts
			auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
				cfg.OnLogin = func(_ context.Context, f writoidc.LoginFacts) { got = append(got, f) }
			})
			if c.entra {
				writoidc.SetEntraForTest(auth)
			}
			env.entraIDToken(t, "pairwise-1", keyTenant, c.oid)
			if _, sess := doCallbackVia(t, auth, auth.CallbackHandler); sess.Sub != "pairwise-1" {
				t.Fatalf("session sub = %q, want pairwise-1", sess.Sub)
			}
			if len(got) != 1 {
				t.Fatalf("OnLogin ran %d times, want 1", len(got))
			}
			f := got[0]
			if f.Sub != "pairwise-1" || f.Issuer != env.httpSrv.URL || f.Email != "pat@corp.example" {
				t.Errorf("facts = %+v, want sub pairwise-1, issuer %s, email pat@corp.example", f, env.httpSrv.URL)
			}
			if f.TenantID != c.wantTenant || f.ObjectID != c.wantO {
				t.Errorf("tenant/object = %q/%q, want %q/%q", f.TenantID, f.ObjectID, c.wantTenant, c.wantO)
			}
		})
	}
}

// loginFactsRoles signs a sign-in whose token carries the given roles and groups claims and returns what
// OnLogin was told.
func loginFactsRoles(t *testing.T, roles, groups []string) writoidc.LoginFacts {
	t.Helper()
	env := newIdPEnv(t)
	var got []writoidc.LoginFacts
	auth := env.newRoleMappingAuth(t, nil, "", nil, nil, func(cfg *writoidc.Config) {
		cfg.OnLogin = func(_ context.Context, f writoidc.LoginFacts) { got = append(got, f) }
	})
	env.buildIDTokenWithRoles(t, "sub-roles", "pat@corp.example", roles, groups, roleCallbackNonce, time.Now().Add(time.Hour))
	if _, sess := doCallbackVia(t, auth, auth.CallbackHandler); sess.Sub != "sub-roles" {
		t.Fatalf("session sub = %q, want sub-roles", sess.Sub)
	}
	if len(got) != 1 {
		t.Fatalf("OnLogin ran %d times, want 1", len(got))
	}
	return got[0]
}

// TestCallbackLoginFactsCarryTheRolesClaim pins the roles claim handed to OnLogin: on its own, trimmed,
// lowercased, deduplicated and sorted as the groups are, and empty when the token carries none.
func TestCallbackLoginFactsCarryTheRolesClaim(t *testing.T) {
	f := loginFactsRoles(t, []string{" Wardyn.Admin ", "wardyn.admin", "Auditor"}, []string{"eng"})
	if want := []string{"auditor", "wardyn.admin"}; fmt.Sprint(f.Roles) != fmt.Sprint(want) {
		t.Errorf("Roles = %v, want %v", f.Roles, want)
	}
	if want := []string{"auditor", "eng", "wardyn.admin"}; fmt.Sprint(f.Groups) != fmt.Sprint(want) {
		t.Errorf("Groups = %v, want the roles folded in with the groups: %v", f.Groups, want)
	}
	if none := loginFactsRoles(t, nil, []string{"eng"}); len(none.Roles) != 0 {
		t.Errorf("Roles = %v with no roles claim, want none", none.Roles)
	}
}

// TestCallbackLoginFactsCapTheRolesClaim pins that a roles claim past the groups byte budget is cut as the
// groups are, and that the snapshot is then marked partial.
func TestCallbackLoginFactsCapTheRolesClaim(t *testing.T) {
	var roles []string
	for i := 0; i < 400; i++ {
		roles = append(roles, fmt.Sprintf("role-%04d-padding-padding-padding", i))
	}
	f := loginFactsRoles(t, roles, nil)
	if len(f.Roles) == 0 || len(f.Roles) >= len(roles) {
		t.Fatalf("kept %d of %d roles, want a cut list", len(f.Roles), len(roles))
	}
	if !f.GroupsTruncated {
		t.Error("GroupsTruncated = false, want true: the roles were cut")
	}
	var used int
	for _, r := range f.Roles {
		used += len(r) + 3
	}
	if used > writoidc.MaxSessionGroupsBytesForTest {
		t.Errorf("kept roles cost %d bytes, over the %d budget", used, writoidc.MaxSessionGroupsBytesForTest)
	}
}
