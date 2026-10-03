// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// PIN for the residue of the boot audit-chain canary finding: the canary ran on
// the MIGRATE pool only, so the posture wardynd's own boot log recommends —
// WARDYN_PG_MIGRATE_DSN, migrator role separate from app role — was the one
// posture it did not cover.
//
// db.Migrate, and so the canary at its tail, runs on the migrate pool. Every
// real audit write goes through the APP pool: a different role, a different
// search_path, different privileges. A canary that only ever ran as the migrator
// proved the chain works for a connection nothing audits on, and a split-role
// boot could start clean while the app pool's very next audit write came back
// unchained — precisely the failure class the canary exists to convert from
// post-hoc to boot-time.
//
// The probe builds that divergence the way a real deployment reaches it, through
// search_path: the migrate DSN points at a fully migrated schema, and the app
// DSN at a schema holding a SHADOWING audit_events with no chain trigger on it.
// Both schemas are dropped on cleanup; the lane's own audit_events is never
// touched.
//
// Guarded by WARDYN_TEST_PG; skipped cleanly when unset.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

func TestConnectAndMigrate_CanaryRunsOnTheAppPoolNotJustTheMigrator(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the split-role boot canary test")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot derive two schema-scoped DSNs from it")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	// t.Cleanup, not defer: a deferred Close runs BEFORE the cleanups below, and
	// the schema drops need this pool. LIFO ordering makes it last.
	t.Cleanup(admin.Close)

	stamp := time.Now().UnixNano() % 1_000_000_000
	migrateSchema := fmt.Sprintf("wardyn_bc_m_%d", stamp)
	appSchema := fmt.Sprintf("wardyn_bc_a_%d", stamp)
	for _, s := range []string{migrateSchema, appSchema} {
		if _, err := admin.Exec(ctx, `CREATE SCHEMA `+s); err != nil {
			t.Fatalf("create schema %s: %v", s, err)
		}
	}
	t.Cleanup(func() {
		for _, s := range []string{migrateSchema, appSchema} {
			if _, err := admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+s+` CASCADE`); err != nil {
				t.Logf("cleanup drop schema %s: %v", s, err)
			}
		}
	})

	schemaDSN := func(schema string) string {
		v := *u
		q := v.Query()
		q.Set("search_path", schema)
		v.RawQuery = q.Encode()
		return v.String()
	}

	// NO chain trigger — the drift, and the state 0058 exists to
	// repair. Catalog checks against the migrated schema still pass; the write
	// the app pool would actually make does not chain.
	if _, err := admin.Exec(ctx, `CREATE TABLE `+appSchema+`.audit_events (
		id UUID PRIMARY KEY, seq BIGSERIAL, time TIMESTAMPTZ NOT NULL DEFAULT now(),
		run_id UUID, actor_type TEXT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL,
		target TEXT, outcome TEXT NOT NULL, source_ip TEXT, data JSONB,
		prev_hash TEXT, row_hash TEXT)`); err != nil {
		t.Fatalf("create the shadowing audit_events in %s: %v", appSchema, err)
	}

	// The append function the app role calls exists and does not chain either: since 0108 every audit
	// write goes through audit_append, and a role that cannot call it is refused earlier, by name, by the
	// privilege check (TestConnectAndMigrate_RefusesAnAppRoleWithoutExecuteOnAuditAppend).
	if _, err := admin.Exec(ctx, `CREATE FUNCTION `+appSchema+`.audit_append(
			p_id uuid, p_time timestamptz, p_run_id uuid, p_actor_type text, p_actor text,
			p_action text, p_target text, p_outcome text, p_source_ip text, p_data jsonb)
		RETURNS TABLE (seq bigint, recorded_at timestamptz, prev_hash text, row_hash text)
		LANGUAGE sql AS $$
			INSERT INTO `+appSchema+`.audit_events (id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data)
			VALUES (p_id, p_time, p_run_id, p_actor_type, p_actor, p_action, p_target, p_outcome, p_source_ip, p_data)
			RETURNING audit_events.seq, now(), audit_events.prev_hash, audit_events.row_hash $$`); err != nil {
		t.Fatalf("create the shadowing audit_append in %s: %v", appSchema, err)
	}

	pool, err := connectAndMigrate(t.Context(), schemaDSN(appSchema), schemaDSN(migrateSchema), 30*time.Second, 2*time.Minute, false)
	if pool != nil {
		pool.Close()
	}
	if err == nil {
		t.Fatalf("connectAndMigrate returned a usable pool for a split-role boot whose APP role sees an audit_events "+
			"with no chain trigger on it. db.Migrate ran on the migrate pool (%s), so its canary proved the chain for "+
			"the MIGRATOR; every audit row this process writes would go to %s.audit_events and come back unchained, "+
			"which the verify sweep reports as a broken log for as long as the process runs",
			migrateSchema, appSchema)
	}
	if !strings.Contains(err.Error(), "app role") {
		t.Fatalf("error = %q, want the refusal to name the app role, so an operator knows WHICH of the two DSNs to look at", err)
	}

	// SCOPED: the same split-role boot with both DSNs on the migrated schema
	// must still come up. The canary is a refusal for a chain that does not
	// chain, not a tax on the split-role posture.
	ok, err := connectAndMigrate(t.Context(), schemaDSN(migrateSchema), schemaDSN(migrateSchema), 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("a split-role boot on a healthy schema: %v, want it to come up", err)
	}
	ok.Close()
}

// TestConnectAndMigrate_RefusesAnAppRoleWithoutExecuteOnAuditAppend: since 0108 every audit row is
// appended by audit_append, so a serving role that cannot EXECUTE it would start clean and then lose every
// audit write to the spool and refuse every credential mint. The boot refuses, naming the GRANT, and comes
// up once the grant exists.
func TestConnectAndMigrate_RefusesAnAppRoleWithoutExecuteOnAuditAppend(t *testing.T) {
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set; skipping the app-role privilege boot test")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot derive a role-scoped DSN from it")
	}
	ctx := context.Background()
	admin, err := db.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(admin.Close)
	var canCreate bool
	if err := admin.QueryRow(ctx, `SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&canCreate); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !canCreate {
		t.Skip("WARDYN_TEST_PG role cannot CREATE ROLE")
	}

	stamp := time.Now().UnixNano() % 1_000_000_000
	schema := fmt.Sprintf("wardyn_ap_s_%d", stamp)
	role := fmt.Sprintf("wardyn_ap_r_%d", stamp)
	const pw = "ap-test-pw"
	if _, err := admin.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	if _, err := admin.Exec(ctx, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, pw)); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`)
		_, _ = admin.Exec(context.Background(), `DROP OWNED BY `+role)
		_, _ = admin.Exec(context.Background(), `DROP ROLE IF EXISTS `+role)
	})
	scoped := func(user *url.Userinfo) string {
		v := *u
		if user != nil {
			v.User = user
		}
		q := v.Query()
		q.Set("search_path", schema)
		v.RawQuery = q.Encode()
		return v.String()
	}
	migrator := scoped(nil)
	first, err := connectAndMigrate(t.Context(), migrator, "", 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("migrate the schema: %v", err)
	}
	first.Close()
	for _, q := range []string{
		fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, role),
		fmt.Sprintf(`GRANT SELECT ON %s.audit_events TO %s`, schema, role),
	} {
		if _, err := admin.Exec(ctx, q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	app := scoped(url.UserPassword(role, pw))

	pool, err := connectAndMigrate(t.Context(), app, migrator, 30*time.Second, 2*time.Minute, false)
	if pool != nil {
		pool.Close()
	}
	if err == nil {
		t.Fatal("a split-role boot came up with an app role that cannot EXECUTE audit_append")
	}
	for _, want := range []string{"app role", role, "GRANT EXECUTE ON FUNCTION audit_append"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error = %q, want it to contain %q so the operator can act on it", err, want)
		}
	}

	if _, err := admin.Exec(ctx, fmt.Sprintf(
		`GRANT EXECUTE ON FUNCTION %s.audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb) TO %s`, schema, role)); err != nil {
		t.Fatalf("grant execute: %v", err)
	}
	ok, err := connectAndMigrate(t.Context(), app, migrator, 30*time.Second, 2*time.Minute, false)
	if err != nil {
		t.Fatalf("a split-role boot with EXECUTE granted: %v, want it to come up", err)
	}
	ok.Close()
}
