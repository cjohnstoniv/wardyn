// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the review finding that AuditDDLProtected's superuser leg read
// pg_roles.rolsuper off the current_user row, so MEMBERSHIP in a superuser role
// was reported as PROTECTED — the overclaim the function exists to prevent, and
// the ordinary managed-Postgres shape (GRANT an admin role TO the app role).
//
// Every role created here is namespaced by nanosecond, the superuser role is
// NOLOGIN so it cannot be connected to, and all of them are dropped on cleanup.
// The one bypass this executes (ALTER TABLE ... DISABLE TRIGGER) runs inside a
// transaction that is ROLLED BACK — Postgres DDL is transactional, so the shared
// audit_events is never left unguarded even if the test fails mid-way.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func TestPG_AuditDDLProtectedFollowsSuperuserRoleMembership(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	var super bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !super {
		t.Skip("WARDYN_TEST_PG role is not a superuser; creating the superuser role this probe grants needs it")
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot derive an app-role DSN from it")
	}

	n := time.Now().UnixNano() % 1_000_000_000
	su := fmt.Sprintf("wardyn_f61_su_%d", n)
	mid := fmt.Sprintf("wardyn_f61_mid_%d", n)
	app := fmt.Sprintf("wardyn_f61_app_%d", n)
	const pw = "f61-probe-pw"

	must := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM ` + app,
			`REVOKE ALL PRIVILEGES ON SCHEMA public FROM ` + app,
			`DROP ROLE IF EXISTS ` + app,
			`DROP ROLE IF EXISTS ` + mid,
			`DROP ROLE IF EXISTS ` + su,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Errorf("cleanup %s: %v", q, err)
			}
		}
	})
	// NOLOGIN: the superuser role exists only to be SET ROLE'd into by its
	// members, never connected to.
	must(`CREATE ROLE ` + su + ` SUPERUSER NOLOGIN`)
	// NOINHERIT on the app role deliberately: 'MEMBER' is the right to SET ROLE,
	// which does not depend on inheritance, and a check that tested inherited
	// PRIVILEGES instead would miss exactly this role.
	must(`CREATE ROLE ` + mid + ` NOLOGIN`)
	must(`CREATE ROLE ` + app + ` LOGIN NOINHERIT PASSWORD '` + pw + `'`)
	// A CHAIN, not a direct grant: app -> mid -> su. Recursive membership is the
	// property under test; a one-level check would pass this and still be wrong.
	must(`GRANT ` + su + ` TO ` + mid)
	must(`GRANT ` + mid + ` TO ` + app)
	must(`GRANT USAGE ON SCHEMA public TO ` + app)
	must(`GRANT SELECT, INSERT ON audit_events TO ` + app) // the documented posture, nothing more

	u.User = url.UserPassword(app, pw)
	appPool, err := Connect(ctx, u.String())
	if err != nil {
		t.Skipf("cannot connect as the app role (pg_hba may not password-auth a new role): %v", err)
	}
	t.Cleanup(appPool.Close)

	t.Run("reported NOT protected", func(t *testing.T) {
		// Precondition: none of the OTHER two legs fire, so a pass here can only
		// come from the superuser leg.
		var ownerLeg, triggerLeg, attrSuper bool
		if err := appPool.QueryRow(ctx, `
			SELECT bool_or(pg_has_role(current_user, c.relowner, 'MEMBER')),
			       bool_or(has_table_privilege(current_user, c.oid, 'TRIGGER')),
			       (SELECT rolsuper FROM pg_roles WHERE rolname = current_user)
			FROM pg_class c WHERE c.relname = 'audit_events' AND c.relkind = 'r'`,
		).Scan(&ownerLeg, &triggerLeg, &attrSuper); err != nil {
			t.Fatalf("read the other legs: %v", err)
		}
		if ownerLeg || triggerLeg || attrSuper {
			t.Fatalf("precondition: owner=%v trigger=%v rolsuper=%v — the probe role must trip NONE of these, "+
				"so that the verdict turns only on superuser MEMBERSHIP", ownerLeg, triggerLeg, attrSuper)
		}

		got, err := AuditDDLProtected(ctx, appPool)
		if err != nil {
			t.Fatalf("AuditDDLProtected(app pool): %v", err)
		}
		if got {
			t.Fatalf("a role with an indirect grant chain to a SUPERUSER role (%s -> %s -> %s) is reported PROTECTED. "+
				"cmd/wardynd then logs \"the append-only guard is DDL-protected\" over a role that can SET ROLE and "+
				"ALTER TABLE ... DISABLE TRIGGER — the overclaim this function exists to prevent", app, mid, su)
		}
	})

	t.Run("and the bypass is real", func(t *testing.T) {
		// Executed rather than argued, inside a rolled-back transaction.
		tx, err := appPool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(context.Background()) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SET ROLE `+su); err != nil {
			t.Fatalf("SET ROLE %s as %s: %v — if this is refused the finding's premise is wrong", su, app, err)
		}
		if _, err := tx.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER `+auditAppendOnlyTriggers[0]); err != nil {
			t.Fatalf("DISABLE TRIGGER after SET ROLE: %v", err)
		}
		t.Logf("as %s: SET ROLE %s; ALTER TABLE audit_events DISABLE TRIGGER %s — accepted (rolled back)",
			app, su, auditAppendOnlyTriggers[0])
	})
}

// TestPG_AuditDDLProtectedIgnoresPgWriteAllData records the NEGATIVE result that
// keeps the predicate closed. pg_write_all_data confers INSERT/UPDATE/DELETE on
// every table, which looks like it should defeat an append-only table — but the
// guard is a TRIGGER, not a privilege, and the trigger still fires. So
// membership in it is NOT a bypass and must NOT be added to AuditDDLProtected;
// this test exists so that is a measured fact rather than a comment somebody
// later "fixes".
func TestPG_AuditDDLProtectedIgnoresPgWriteAllData(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	var exists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'pg_write_all_data')`).Scan(&exists); err != nil {
		t.Fatalf("look up pg_write_all_data: %v", err)
	}
	if !exists {
		t.Skip("this Postgres has no pg_write_all_data predefined role (added in 14)")
	}
	var super bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !super {
		t.Skip("WARDYN_TEST_PG role cannot CREATE ROLE")
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN")
	}

	role := fmt.Sprintf("wardyn_f61_wad_%d", time.Now().UnixNano()%1_000_000_000)
	const pw = "f61-wad-pw"
	if _, err := pool.Exec(ctx, `CREATE ROLE `+role+` LOGIN PASSWORD '`+pw+`'`); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`REVOKE ALL PRIVILEGES ON SCHEMA public FROM ` + role,
			`DROP ROLE IF EXISTS ` + role,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Errorf("cleanup %s: %v", q, err)
			}
		}
	})
	if _, err := pool.Exec(ctx, `GRANT pg_write_all_data TO `+role); err != nil {
		t.Fatalf("grant pg_write_all_data: %v", err)
	}
	if _, err := pool.Exec(ctx, `GRANT USAGE ON SCHEMA public TO `+role); err != nil {
		t.Fatalf("grant usage: %v", err)
	}

	u.User = url.UserPassword(role, pw)
	wad, err := Connect(ctx, u.String())
	if err != nil {
		t.Skipf("cannot connect as the pg_write_all_data role: %v", err)
	}
	t.Cleanup(wad.Close)

	assertRefused(t, wad, `UPDATE audit_events SET outcome = 'success'`)
	assertRefused(t, wad, `DELETE FROM audit_events`)

	got, err := AuditDDLProtected(ctx, wad)
	if err != nil {
		t.Fatalf("AuditDDLProtected: %v", err)
	}
	if !got {
		t.Errorf("a pg_write_all_data member is reported NOT protected; the append-only triggers demonstrably still "+
			"refuse its UPDATE and DELETE, so widening the predicate to cover it would be a claim WEAKER than the "+
			"enforcing setup — the opposite error, and just as dishonest (got protected=%v)", got)
	}
}

// assertRefused runs a statement expected to be stopped by the append-only
// trigger, inside a transaction that is rolled back either way.
func assertRefused(t *testing.T, pool *pgxpool.Pool, stmt string) {
	t.Helper()
	ctx := context.Background()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	if _, err := tx.Exec(ctx, stmt); err == nil {
		t.Errorf("%q was ACCEPTED for a pg_write_all_data member; the append-only guard is not what this test assumes", stmt)
	} else {
		t.Logf("%q -> refused: %v", stmt, err)
	}
}
