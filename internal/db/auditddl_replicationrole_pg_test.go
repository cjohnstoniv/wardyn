// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the reopened review finding that AuditDDLProtected's enumeration was
// incomplete: a role granted SET on the session_replication_role parameter
// silences ALL THREE simply-enabled audit triggers and appends whatever it likes,
// while the boot logs the deployment as DDL-protected.
//
// The three legs the function had are about DDL — dropping or disabling a
// trigger. This route touches no DDL at all: `SET session_replication_role =
// 'replica'` makes every 'O' trigger stop firing for the session, which is
// precisely why an operator hardening one to ENABLE ALWAYS ('A') is the
// documented countermeasure. Since PostgreSQL 15 that capability is GRANTable to
// a non-superuser (GRANT SET ON PARAMETER), so it is no longer implied by the
// superuser leg — and it is exactly the kind of narrow grant a DBA hands an
// application role for a bulk load.
//
// The probe role is created, granted, connected as, and dropped; the bypass it
// executes runs inside a transaction that is ROLLED BACK, so the shared
// audit_events never keeps the unchained row.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"
)

func TestPG_AuditDDLProtectedCountsTheReplicationRoleGrant(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	var super bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !super {
		t.Skip("WARDYN_TEST_PG role is not a superuser; GRANT SET ON PARAMETER needs it")
	}
	var pg15 bool
	if err := pool.QueryRow(ctx, `SELECT current_setting('server_version_num')::int >= 150000`).Scan(&pg15); err != nil {
		t.Fatalf("read server version: %v", err)
	}
	if !pg15 {
		t.Skip("GRANT ... ON PARAMETER is PostgreSQL 15+; before that only a superuser can set session_replication_role, which the superuser leg already covers")
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot derive an app-role DSN from it")
	}

	app := fmt.Sprintf("wardyn_f99_app_%d", time.Now().UnixNano()%1_000_000_000)
	const pw = "f99-probe-pw"
	must := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	t.Cleanup(func() {
		for _, q := range []string{
			`REVOKE SET ON PARAMETER session_replication_role FROM ` + app,
			`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM ` + app,
			`REVOKE ALL PRIVILEGES ON SCHEMA public FROM ` + app,
			`DROP ROLE IF EXISTS ` + app,
		} {
			if _, err := pool.Exec(context.Background(), q); err != nil {
				t.Errorf("cleanup %s: %v", q, err)
			}
		}
	})
	must(`CREATE ROLE ` + app + ` LOGIN PASSWORD '` + pw + `'`)
	must(`GRANT USAGE ON SCHEMA public TO ` + app)
	must(`GRANT SELECT, INSERT ON audit_events TO ` + app) // the documented posture…
	must(`GRANT SET ON PARAMETER session_replication_role TO ` + app)

	u.User = url.UserPassword(app, pw)
	appPool, err := Connect(ctx, u.String())
	if err != nil {
		t.Skipf("cannot connect as the app role (pg_hba may not password-auth a new role): %v", err)
	}
	t.Cleanup(appPool.Close)

	// Precondition: NONE of the three original legs fire, so the verdict can
	// only turn on the parameter grant.
	var superLeg, ownerLeg, triggerLeg bool
	if err := appPool.QueryRow(ctx, `
		SELECT bool_or(EXISTS (SELECT 1 FROM pg_roles s WHERE s.rolsuper AND pg_has_role(current_user, s.oid, 'MEMBER'))),
		       bool_or(pg_has_role(current_user, c.relowner, 'MEMBER')),
		       bool_or(has_table_privilege(current_user, c.oid, 'TRIGGER'))
		FROM pg_class c WHERE c.relname = 'audit_events' AND c.relkind = 'r'`,
	).Scan(&superLeg, &ownerLeg, &triggerLeg); err != nil {
		t.Fatalf("read the three original legs: %v", err)
	}
	if superLeg || ownerLeg || triggerLeg {
		t.Fatalf("precondition: super=%v owner=%v trigger=%v — the probe role must trip NONE of these",
			superLeg, ownerLeg, triggerLeg)
	}

	t.Run("the bypass is real", func(t *testing.T) {
		// Executed rather than argued, inside a rolled-back transaction. SET
		// LOCAL scopes the change to the transaction, so the pooled connection
		// is handed back exactly as it was found.
		tx, err := appPool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(context.Background()) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = 'replica'`); err != nil {
			t.Fatalf("SET LOCAL session_replication_role as %s: %v — if this is refused the finding's premise is wrong", app, err)
		}
		var rowHash *string
		if err := tx.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
			VALUES (gen_random_uuid(), 'human', 'forger@example.com', 'test.f99.replica', 'success')
			RETURNING row_hash`).Scan(&rowHash); err != nil {
			t.Fatalf("INSERT under session_replication_role = replica: %v", err)
		}
		if rowHash != nil {
			t.Fatalf("row_hash = %q — the chain trigger still fired, so this deployment's triggers are not simply 'O' "+
				"and the probe is not testing the bypass it claims", *rowHash)
		}
		t.Logf("as %s: SET LOCAL session_replication_role='replica'; INSERT INTO audit_events — accepted, row_hash NULL "+
			"(all three triggers silenced, no DDL), rolled back", app)
	})

	t.Run("and it is reported NOT protected", func(t *testing.T) {
		got, err := AuditDDLProtected(ctx, appPool)
		if err != nil {
			t.Fatalf("AuditDDLProtected(app pool): %v", err)
		}
		if got {
			t.Fatalf("a role holding only INSERT/SELECT plus GRANT SET ON PARAMETER session_replication_role is reported " +
				"PROTECTED. cmd/wardynd then logs \"the append-only guard is DDL-protected\" over a role that just " +
				"appended a forged, unchained row past all three triggers — the overclaim this function exists to prevent")
		}
	})
}
