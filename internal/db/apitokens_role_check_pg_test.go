// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for 0060's api_tokens.role CHECK, and for the half of the decision a
// text-level parity case cannot see: that the constraint is actually enforced
// by the server, and — more importantly — that it does not refuse a role the
// product legitimately stamps.
//
// The second half is the one with teeth. Adding a CHECK to a column the API
// writes re-imports the incident 0053 documents if the Go side ever widens
// first: the write passes validation and then 500s at the database. Here that
// would land on token minting. So this asserts EVERY value in oidc.Roles is
// accepted, derived from the same slice the CHECK is pinned to, rather than a
// list typed out again.

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
)

func TestPG_APITokenRoleCheckAcceptsEveryRealRoleAndRefusesTheRest(t *testing.T) {
	pool, _ := probeSchemaPool(t)
	ctx := context.Background()

	insert := func(role string) error {
		_, err := pool.Exec(ctx, `INSERT INTO api_tokens (id, principal, role, token_sha256)
			VALUES ($1, 'probe@corp.example', $2, $3)`, uuid.New(), role, uuid.NewString())
		return err
	}

	for _, role := range oidc.Roles {
		if err := insert(role); err != nil {
			t.Errorf("minting a token stamped %q was REFUSED by the database: %v\n"+
				"every role oidc.Roles holds is a role a session can carry, so it is a role POST /me/tokens must be "+
				"able to stamp; a CHECK that refuses one turns the mint into a 500 for exactly the persona that "+
				"holds it — the shape of the incident 0053 documents", role, err)
		}
	}

	for _, bogus := range []string{"", "Admin", "security-admin", "superadmin", "owner"} {
		err := insert(bogus)
		if err == nil {
			t.Errorf("api_tokens.role accepted %q; 0045 shipped this column unconstrained on the argument that any "+
				"value but admin was inert, which stopped being true when security_admin became a privilege — the "+
				"CHECK is the drift guard that replaced that argument", bogus)
			continue
		}
		if got := sqlState(err); got != "23514" {
			t.Errorf("insert of %q failed with SQLSTATE %q, want 23514 check_violation: %v", bogus, got, err)
		}
	}
}
