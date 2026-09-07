// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the reopened finding that AuditDDLProtected's FOURTH leg — the
// session_replication_role one — was the only leg that did not follow role
// membership, so the exact shape the first three legs were rewritten to catch
// walked past it.
//
// The shape: GRANT SET ON PARAMETER session_replication_role TO admin;
// GRANT admin TO app, with app NOINHERIT. has_parameter_privilege(app, …) is
// then FALSE — app holds no such grant of its own and inherits nothing — so the
// pre-fix leg reported the deployment PROTECTED. app can nonetheless run
// SET ROLE admin; SET session_replication_role = 'replica'; RESET ROLE and, back
// as ITSELF, append rows past all three simply-enabled audit triggers.
//
// This test executes that bypass rather than arguing it (the INSERT comes back
// with row_hash NULL, proving the chain trigger did not fire) and then asserts
// the verdict. Everything runs inside a transaction that is ROLLED BACK, so the
// shared audit_events keeps no unchained row; the two probe roles are created
// and dropped by the test.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestPG_AuditDDLProtectedFollowsMembershipToTheReplicationRoleGrant(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	var super bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	urlDSN := err == nil && u.Scheme != "" && u.Host != ""
	// GRANT SET ON PARAMETER is a superuser-only grant, so superuser (not merely
	// CREATEROLE) is what makes this lane able to satisfy every precondition.
	mustNotSkip := probeMustNotSkip(t, super, urlDSN)
	if !super {
		skipOrFatal(t, mustNotSkip, "WARDYN_TEST_PG role is not a superuser; GRANT SET ON PARAMETER needs it")
	}
	if !urlDSN {
		skipOrFatal(t, mustNotSkip, "WARDYN_TEST_PG is not a URL-form DSN; cannot derive an app-role DSN from it")
	}
	var pg15 bool
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int >= 150000`).Scan(&pg15); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	if !pg15 {
		t.Skip("GRANT ... ON PARAMETER is PostgreSQL 15+; before that only a superuser can set session_replication_role, " +
			"which the superuser leg already covers")
	}

	suffix := fmt.Sprintf("%d", time.Now().UnixNano()%1_000_000_000)
	adm := "wardyn_f330_adm_" + suffix
	app := "wardyn_f330_app_" + suffix
	const pw = "f330-probe-pw"
	must := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`REVOKE ` + adm + ` FROM ` + app,
			`REVOKE SET ON PARAMETER session_replication_role FROM ` + adm,
			`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM ` + app,
			`REVOKE ALL PRIVILEGES ON SCHEMA public FROM ` + app,
			`DROP ROLE IF EXISTS ` + app,
			`DROP ROLE IF EXISTS ` + adm,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Errorf("cleanup %s: %v", q, err)
			}
		}
	})
	// The admin role holds the parameter grant and nothing else; the app role
	// holds the ordinary 0007 posture and is NOINHERIT, so it gets the admin
	// role's capability only by SET ROLE — the managed-Postgres shape.
	must(`CREATE ROLE ` + adm + ` NOLOGIN`)
	must(`GRANT SET ON PARAMETER session_replication_role TO ` + adm)
	must(`CREATE ROLE ` + app + ` NOINHERIT LOGIN PASSWORD '` + pw + `'`)
	must(`GRANT ` + adm + ` TO ` + app)
	must(`GRANT USAGE ON SCHEMA public TO ` + app)
	must(`GRANT SELECT, INSERT ON audit_events TO ` + app)

	u.User = url.UserPassword(app, pw)
	appPool, err := Connect(ctx, u.String())
	if err != nil {
		skipOrFatal(t, mustNotSkip, "cannot connect as the app role (pg_hba may not password-auth a new role): %v", err)
	}
	t.Cleanup(appPool.Close)

	// PRECONDITION, and it is the whole finding: none of the first three legs
	// fire, AND the pre-fix fourth leg's own question answers false — so a
	// verdict of PROTECTED here is exactly what the pre-fix code returned.
	var superLeg, ownerLeg, triggerLeg, ownGrant, memberOfAdm bool
	if err := appPool.QueryRow(ctx, `
		SELECT bool_or(EXISTS (SELECT 1 FROM pg_roles s WHERE s.rolsuper AND pg_has_role(current_user, s.oid, 'MEMBER'))),
		       bool_or(pg_has_role(current_user, c.relowner, 'MEMBER')),
		       bool_or(has_table_privilege(current_user, c.oid, 'TRIGGER')),
		       bool_or(has_parameter_privilege(current_user, 'session_replication_role', 'SET')),
		       bool_or(pg_has_role(current_user, '`+adm+`', 'MEMBER'))
		FROM pg_class c WHERE c.relname = 'audit_events' AND c.relkind = 'r'`,
	).Scan(&superLeg, &ownerLeg, &triggerLeg, &ownGrant, &memberOfAdm); err != nil {
		t.Fatalf("read the four legs: %v", err)
	}
	if superLeg || ownerLeg || triggerLeg {
		t.Fatalf("precondition: super=%v owner=%v trigger=%v — the probe role must trip NONE of these",
			superLeg, ownerLeg, triggerLeg)
	}
	if ownGrant {
		t.Fatalf("precondition: has_parameter_privilege(%s, 'session_replication_role', 'SET') = true — the probe role "+
			"was supposed to reach the grant only THROUGH %s, so this is not the membership case the finding names", app, adm)
	}
	if !memberOfAdm {
		t.Fatalf("precondition: %s is not a MEMBER of %s; the GRANT did not take", app, adm)
	}

	t.Run("the bypass is real", func(t *testing.T) {
		conn, err := appPool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(context.Background()) //nolint:errcheck

		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+adm); err != nil {
			t.Fatalf("SET LOCAL ROLE %s as %s: %v — if this is refused the finding's premise is wrong", adm, app, err)
		}
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
			t.Fatalf("SET LOCAL session_replication_role while SET ROLE %s: %v", adm, err)
		}
		if _, err := tx.Exec(ctx, `RESET ROLE`); err != nil {
			t.Fatalf("RESET ROLE: %v", err)
		}
		var who, mode string
		if err := tx.QueryRow(ctx,
			`SELECT current_user, current_setting('session_replication_role')`).Scan(&who, &mode); err != nil {
			t.Fatalf("read current_user/session_replication_role: %v", err)
		}
		if who != app || mode != "replica" {
			t.Fatalf("after RESET ROLE: current_user=%q session_replication_role=%q — want %q/replica; the bypass did "+
				"not survive back to the app role", who, mode, app)
		}
		var rowHash *string
		if err := tx.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
			VALUES (gen_random_uuid(), 'human', 'forger@example.com', 'test.f330.replica', 'success')
			RETURNING row_hash`).Scan(&rowHash); err != nil {
			t.Fatalf("INSERT under session_replication_role = replica: %v", err)
		}
		if rowHash != nil {
			t.Fatalf("row_hash = %q — the chain trigger still fired, so this deployment's triggers are not simply 'O' "+
				"and the probe is not testing the bypass it claims", *rowHash)
		}
		t.Logf("as %s: SET ROLE %s; SET session_replication_role='replica'; RESET ROLE; INSERT INTO audit_events — "+
			"accepted as %s with row_hash NULL (all three triggers silenced, no DDL), rolled back", app, adm, app)
	})

	t.Run("and it is reported NOT protected", func(t *testing.T) {
		got, err := AuditDDLProtected(ctx, appPool)
		if err != nil {
			t.Fatalf("AuditDDLProtected(app pool): %v", err)
		}
		if got {
			t.Fatalf("a role that reaches SET ON PARAMETER session_replication_role through membership in %s is "+
				"reported PROTECTED. It just appended a forged, unchained row past all three triggers, so the boot "+
				"log's \"the append-only guard is DDL-protected\" is a claim stronger than the role setup — the one "+
				"thing this function promises never to do", adm)
		}
	})
}
