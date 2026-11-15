// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// An identity row unbound and bound to a new sub: erasing the new holder names the released principal as not
// erased, and erasing the released principal clears it from the binding history. Guarded by WARDYN_TEST_PG.

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

func TestPG_ErasePerson_ClearsAReleasedPrincipalFromTheBindingHistory(t *testing.T) {
	_, srv, pg, _ := identityRig(t)
	ctx := t.Context()
	oid := uuid.NewString()
	released, holder := "sub-old-"+oid, "sub-new-"+oid
	seedIdentity(t, pg, released, oid, "")
	row, err := pg.GetIdentityByObject(ctx, identIssuer, identTenant, oid)
	if err != nil {
		t.Fatal(err)
	}
	if err := pg.UnbindIdentity(ctx, row.ID, released); err != nil {
		t.Fatal(err)
	}
	seedIdentity(t, pg, holder, oid, "")
	sa := ssoSession(t, "sec-1", "sec@corp.example", oidc.RoleSecurityAdmin)
	named := func() int {
		t.Helper()
		var n int
		if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM principal_identity_unbinds WHERE identity_id = $1 AND principal = $2`, row.ID, released).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}

	w := doSSO(t, srv, http.MethodPost, "/api/v1/people/"+holder+"/erasure", sa, erasureBody("audit_personal_fields"))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"`+released+`"`) {
		t.Fatalf("erasing the new holder = %d %s, want 200 naming the released principal as not erased", w.Code, w.Body)
	}
	if named() != 1 {
		t.Fatal("erasing the new holder cleared the released principal's binding history")
	}

	w = doSSO(t, srv, http.MethodPost, "/api/v1/people/"+released+"/erasure", sa, erasureBody("audit_personal_fields"))
	if w.Code != http.StatusOK {
		t.Fatalf("erasing the released principal = %d %s, want 200", w.Code, w.Body)
	}
	if named() != 0 {
		t.Fatal("the binding history still names the erased principal")
	}
	var kept int
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM principal_identity_unbinds WHERE identity_id = $1 AND principal IS NULL`, row.ID).Scan(&kept); err != nil || kept != 1 {
		t.Fatalf("%d nameless history rows (%v), want the unbind kept with no name", kept, err)
	}
}
