// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// F11 PROBE — destination: internal/db/auditddl_f11_probe_pg_test.go
//
// db.AuditDDLProtected is 0% covered (docs/TEST-GAPS.md) because every live
// lane connects as the container superuser and the split-role mode is never
// exercised. This probe manufactures the split: it creates a throwaway LOGIN
// role with only INSERT+SELECT on audit_events (0007's intended app role),
// connects as it, and checks what the boot-time claim and the DB actually do.
//
// Needs WARDYN_TEST_PG in URL form and a role with CREATEROLE/superuser (CI's
// pg lane is `postgres`). Skips otherwise. Cleans the role up.
//
// Expected result on feat/v0.7-profiles @ fa910735:
//
//	TestPG_ProbeF11_AuditDDLProtected/…                     GREEN except the last subtest
//	TestPG_ProbeF11_AuditDDLProtected/TRIGGER_privilege…    RED (hypothesis H4)
//	TestPG_ProbeF11_DroppedChainTriggerIsRestoredByMigrate  RED (hypothesis H2)
package db

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func sqlState(err error) string {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code
	}
	return ""
}

func TestPG_ProbeF11_AuditDDLProtected(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	var canCreateRole bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&canCreateRole); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !canCreateRole {
		t.Skip("WARDYN_TEST_PG role cannot CREATE ROLE; the split-role probe needs it")
	}
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot derive an app-role DSN from it")
	}

	// 1. Honesty on the owning/migrating role: it must NOT be reported protected.
	if got, err := AuditDDLProtected(ctx, pool); err != nil {
		t.Fatalf("AuditDDLProtected(owner pool): %v", err)
	} else if got {
		t.Fatalf("AuditDDLProtected reported PROTECTED for the role that migrates/owns audit_events (single-DSN mode) — an overclaim")
	}

	role := fmt.Sprintf("wardyn_f11_app_%d", time.Now().UnixNano()%1_000_000_000)
	const pw = "f11-probe-pw"
	must := func(q string) {
		t.Helper()
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	must(fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, pw))
	t.Cleanup(func() {
		for _, q := range []string{
			`REVOKE ALL PRIVILEGES ON ALL TABLES IN SCHEMA public FROM ` + role,
			`REVOKE ALL PRIVILEGES ON ALL SEQUENCES IN SCHEMA public FROM ` + role,
			`REVOKE ALL PRIVILEGES ON SCHEMA public FROM ` + role,
			`DROP ROLE IF EXISTS ` + role,
		} {
			if _, err := pool.Exec(ctx, q); err != nil {
				t.Logf("cleanup %s: %v", q, err)
			}
		}
	})
	must(`GRANT USAGE ON SCHEMA public TO ` + role)
	must(`GRANT SELECT, INSERT ON audit_events TO ` + role)
	must(`GRANT USAGE ON ALL SEQUENCES IN SCHEMA public TO ` + role)

	u.User = url.UserPassword(role, pw)
	app, err := Connect(ctx, u.String())
	if err != nil {
		t.Skipf("cannot connect as the app role (pg_hba may not password-auth a new role): %v", err)
	}
	t.Cleanup(app.Close)

	t.Run("split app role is reported protected", func(t *testing.T) {
		got, err := AuditDDLProtected(ctx, app)
		if err != nil {
			t.Fatalf("AuditDDLProtected(app pool): %v", err)
		}
		if !got {
			t.Fatalf("a non-owner, non-superuser role with only INSERT+SELECT is reported NOT protected")
		}
	})

	t.Run("app role can still append a chained row through the real statement", func(t *testing.T) {
		tx, err := app.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, AuditChainLockKey); err != nil {
			t.Fatalf("advisory lock as app role: %v", err)
		}
		var rowHash string
		if err := tx.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
			VALUES (gen_random_uuid(), 'system', 'f11-app-role', 'test.ddl.probe', 'success')
			RETURNING COALESCE(row_hash,'')`).Scan(&rowHash); err != nil {
			t.Fatalf("INSERT as app role: %v (INSERT+SELECT+sequence USAGE should be enough)", err)
		}
		if rowHash == "" {
			t.Fatal("row inserted by the app role has no row_hash; the 0047 trigger did not run for it")
		}
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit: %v", err)
		}
	})

	t.Run("DDL bypasses are refused with insufficient_privilege", func(t *testing.T) {
		for _, q := range []string{
			`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_update`,
			`ALTER TABLE audit_events DISABLE TRIGGER audit_events_no_truncate`,
			`ALTER TABLE audit_events DISABLE TRIGGER audit_events_chain`,
			`ALTER TABLE audit_events DISABLE TRIGGER ALL`,
			`DROP TRIGGER audit_events_no_update ON audit_events`,
			`DROP TRIGGER audit_events_chain ON audit_events`,
			`DROP TABLE audit_events`,
			`ALTER TABLE audit_events DROP COLUMN row_hash`,
			`CREATE OR REPLACE FUNCTION audit_events_append_only() RETURNS trigger AS $$ BEGIN RETURN NEW; END $$ LANGUAGE plpgsql`,
			`CREATE OR REPLACE FUNCTION audit_row_hash(prev TEXT, p_id UUID, p_time TIMESTAMPTZ, p_run_id UUID, p_actor_type TEXT, p_actor TEXT, p_action TEXT, p_target TEXT, p_outcome TEXT, p_source_ip TEXT, p_data JSONB) RETURNS TEXT LANGUAGE sql STABLE AS $$ SELECT 'forged' $$`,
			`ALTER TABLE audit_events OWNER TO ` + role,
		} {
			_, err := app.Exec(ctx, q)
			if err == nil {
				t.Errorf("app role was ALLOWED to run: %s", q)
				continue
			}
			if code := sqlState(err); code != "42501" {
				t.Errorf("%s: refused with SQLSTATE %q (%v), want 42501 insufficient_privilege", q, code, err)
			}
		}
	})

	t.Run("DML bypasses are refused", func(t *testing.T) {
		for _, q := range []string{
			`UPDATE audit_events SET actor = 'x' WHERE actor = 'f11-app-role'`,
			`DELETE FROM audit_events WHERE actor = 'f11-app-role'`,
			`TRUNCATE audit_events`,
		} {
			_, err := app.Exec(ctx, q)
			if err == nil {
				t.Errorf("app role was ALLOWED to run: %s", q)
				continue
			}
			// Either the privilege is missing (42501) or the trigger raised (P0001); both keep the row.
			if code := sqlState(err); code != "42501" && code != "P0001" {
				t.Errorf("%s: refused with SQLSTATE %q, want 42501 or P0001", q, code)
			}
		}
		var enabled string
		if err := pool.QueryRow(ctx, `SELECT tgenabled FROM pg_trigger WHERE tgname = 'audit_events_no_update' AND tgrelid = 'audit_events'::regclass`).Scan(&enabled); err != nil {
			t.Fatalf("read trigger state: %v", err)
		}
		if enabled != "O" {
			t.Errorf("audit_events_no_update tgenabled = %q after the probe, want 'O'", enabled)
		}
	})

	t.Run("TRIGGER privilege is not part of the claim (H4)", func(t *testing.T) {
		must(`GRANT TRIGGER ON audit_events TO ` + role)
		t.Cleanup(func() { pool.Exec(ctx, `REVOKE TRIGGER ON audit_events FROM `+role) }) //nolint:errcheck
		got, err := AuditDDLProtected(ctx, app)
		if err != nil {
			t.Fatalf("AuditDDLProtected(app pool + TRIGGER): %v", err)
		}
		if got {
			t.Errorf("KNOWN GAP (F11 H4): the app role now holds TRIGGER on audit_events (it may CREATE a BEFORE INSERT trigger that " +
				"fires after audit_events_chain — alphabetical order — and overwrite NEW.row_hash/prev_hash), yet AuditDDLProtected still " +
				"reports protected; the check reads ownership/superuser only, never has_table_privilege(..., 'TRIGGER')")
		}
	})
}

// TestPG_ProbeF11_DroppedChainTriggerIsRestoredByMigrate — hypothesis H2.
//
// An owner/superuser drops the 0047 trigger. Migrate() records 0047 as applied
// and skips it on every later boot (isMigrationApplied), and nothing at boot
// reads pg_trigger — so the trigger stays gone across restarts and every row
// written from then on is unchained (which H1 shows the sweep never reports).
// The desired property asserted here — the next Migrate (or boot) restores or
// at least refuses without the trigger — does not hold, so expected RED. The
// trigger is put back afterwards by re-executing 0047 (idempotent DDL).
func TestPG_ProbeF11_DroppedChainTriggerIsRestoredByMigrate(t *testing.T) {
	pool := pgPool(t)
	ctx := context.Background()

	triggerPresent := func() bool {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM pg_trigger WHERE tgname = 'audit_events_chain' AND tgrelid = 'audit_events'::regclass AND tgenabled = 'O'`).Scan(&n); err != nil {
			t.Fatalf("read pg_trigger: %v", err)
		}
		return n == 1
	}
	if !triggerPresent() {
		t.Fatal("precondition: audit_events_chain trigger missing or disabled before the probe ran")
	}
	if _, err := pool.Exec(ctx, `DROP TRIGGER audit_events_chain ON audit_events`); err != nil {
		t.Skipf("cannot DROP TRIGGER as this role (%v); the probe needs table ownership", err)
	}
	t.Cleanup(func() {
		sql, err := migrationFS.ReadFile("migrations/0047_audit_hash_chain.sql")
		if err != nil {
			t.Errorf("read 0047: %v", err)
			return
		}
		if _, err := pool.Exec(ctx, string(sql)); err != nil {
			t.Errorf("re-apply 0047 to restore the trigger: %v", err)
		}
		if !triggerPresent() {
			t.Errorf("audit_events_chain trigger is STILL missing after re-applying 0047 — later chain tests in this run will see unchained rows")
		}
	})

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after DROP TRIGGER: %v", err)
	}
	if !triggerPresent() {
		t.Fatalf("KNOWN GAP (F11 H2): a dropped audit_events_chain trigger is NOT restored by the next Migrate — 0047 is recorded in " +
			"schema_migrations and skipped — and nothing at boot inspects pg_trigger; every row written from now on is unchained")
	}
}
