// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package oidc_test

import (
	"context"
	"testing"

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
