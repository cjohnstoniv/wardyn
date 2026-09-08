// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PINS for the two review findings against 0057_audit_chain_security_definer.sql
// (fixed by 0058_audit_chain_schema_qualified.sql):
//
//	F024 — the pinned `search_path = pg_catalog, public` made the SECURITY
//	       DEFINER body resolve `audit_events` and `audit_row_hash` in public
//	       rather than in the schema the migrations actually ran against. On a
//	       deployment with nothing in public that is `relation "audit_events"
//	       does not exist`, raised from inside the trigger after Migrate had
//	       already reported success; where public happens to hold another
//	       Wardyn schema it is quieter and worse — the head read crosses into
//	       the wrong table and the chain never links.
//
//	F098 — that same path omitted `pg_temp`, and PostgreSQL searches the
//	       session temp schema FIRST for relation names when it is not listed.
//	       A caller could therefore `CREATE TEMP TABLE audit_events(...)`,
//	       seed it, and have the definer-privileged head read come back with a
//	       prev_hash of the caller's choosing — the one thing
//	       docs/OPERATIONS.md promises no writer can pick.
//
// Both need a live Postgres (WARDYN_TEST_PG); both skip cleanly without one.
//
// The UPGRADE boundary those same migrations cross — rows already chained by
// 0047's trigger, then 0056/0057/0058 rewriting the function underneath them —
// is pinned in internal/store, not here: it needs both the databaseBefore
// harness and store.PG.VerifyAuditChain, and internal/store imports internal/db
// so this package cannot reach either. See
// TestPG_AuditChainSurvivesTheTriggerRewriteOnAPopulatedDatabase
// (internal/store/auditchain_upgrade_pg_test.go). Everything in THIS file
// starts from a fully migrated schema (F023).

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// probeSchemaPool migrates a COMPLETE Wardyn schema into a throwaway namespace
// and returns a pool whose search_path points at it. Nothing it does can reach
// the lane's own audit_events, so a test is free to install triggers on the
// table or leave rows behind. The schema is dropped on cleanup.
func probeSchemaPool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	base := pgPool(t) // also proves the default-schema install still migrates
	ctx := context.Background()

	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot point a connection at another schema")
	}
	schema := fmt.Sprintf("wardyn_sp_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := base.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create probe schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	})

	q := u.Query()
	q.Set("search_path", schema) // the ONLY thing that differs from the lane's own DSN
	u.RawQuery = q.Encode()

	pool, err := Connect(ctx, u.String())
	if err != nil {
		t.Fatalf("connect with search_path=%s: %v", schema, err)
	}
	t.Cleanup(pool.Close)

	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate into schema %s: %v", schema, err)
	}
	return pool, schema
}

// TestPG_ChainTriggerWorksOutsideThePublicSchema is the F024 pin. It migrates a
// COMPLETE schema into a throwaway namespace — the shared-corporate-Postgres
// posture, reachable with nothing but `search_path` on the connection — and
// appends two audit rows through the real trigger.
func TestPG_ChainTriggerWorksOutsideThePublicSchema(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	// Everything the migrations create is unqualified, so it must all have
	// landed in the probe schema. If it did not, the rest of the test would be
	// asserting against the lane's own public schema and would pass vacuously.
	var landed int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		 WHERE n.nspname = $1 AND c.relname = 'audit_events'`, schema).Scan(&landed); err != nil {
		t.Fatalf("locate audit_events: %v", err)
	}
	if landed != 1 {
		t.Fatalf("audit_events did not land in schema %s (found %d); the probe is not testing what it claims", schema, landed)
	}

	append1 := func(action string) (rowHash, prevHash string) {
		t.Helper()
		err := pool.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
			VALUES (gen_random_uuid(), 'system', 'searchpath-probe', $1, 'success')
			RETURNING COALESCE(row_hash, ''), COALESCE(prev_hash, '')`, action).Scan(&rowHash, &prevHash)
		if err != nil {
			t.Fatalf("INSERT into audit_events in schema %s: %v\n"+
				"the chain trigger runs SECURITY DEFINER; a search_path pinned to a schema the migrations did NOT "+
				"run against makes every audit write on this deployment fail from inside the trigger, while Migrate "+
				"still reports success (F024)", schema, err)
		}
		if rowHash == "" {
			t.Fatal("row appended outside public has no row_hash; the chain trigger did not run for it")
		}
		return rowHash, prevHash
	}

	first, firstPrev := append1("test.searchpath.nonpublic.1")
	if firstPrev != "" {
		t.Fatalf("the first row of a FRESH schema chained to prev_hash %q; the head read reached another schema's audit_events", firstPrev)
	}
	_, secondPrev := append1("test.searchpath.nonpublic.2")
	if secondPrev != first {
		t.Fatalf("prev_hash of the second row in schema %s is %q, want the first row's row_hash %q — "+
			"the definer-side head read resolved audit_events somewhere other than the schema the trigger is attached to (F024)",
			schema, secondPrev, first)
	}
}

// TestPG_ChainTriggerIgnoresAShadowingTempTable is the F098 pin. PostgreSQL's
// CREATE FUNCTION documentation names this exact configuration — a SECURITY
// DEFINER function with unqualified relation references and a SET clause that
// does not end in pg_temp — as the subvertible one.
func TestPG_ChainTriggerIgnoresAShadowingTempTable(t *testing.T) {
	pgPool(t) // migrate the lane's database, then take our OWN session
	ctx := context.Background()

	// A dedicated connection, never a pooled one: the temp table below must not
	// outlive this test on a connection handed back to the pool.
	conn, err := pgx.Connect(ctx, os.Getenv("WARDYN_TEST_PG"))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer conn.Close(context.Background()) //nolint:errcheck

	// Resolve the real table BEFORE the shadow exists — afterwards
	// 'audit_events'::regclass would answer with the temp one.
	var qualified string
	if err := conn.QueryRow(ctx, `
		SELECT quote_ident(n.nspname) || '.' || quote_ident(c.relname)
		FROM pg_class c JOIN pg_namespace n ON n.oid = c.relnamespace
		WHERE c.oid = 'audit_events'::regclass`).Scan(&qualified); err != nil {
		t.Fatalf("qualify audit_events: %v", err)
	}
	// Guarantee the real chain has a head, so "prev_hash is the real head" is
	// not satisfiable by an empty table.
	if _, err := conn.Exec(ctx, `INSERT INTO `+qualified+` (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'searchpath-probe', 'test.searchpath.seed', 'success')`); err != nil {
		t.Fatalf("seed the real chain: %v", err)
	}

	forged := "dead" + strings.Repeat("beef", 15) // 64 hex chars, the shape of a row_hash
	if _, err := conn.Exec(ctx, `CREATE TEMP TABLE audit_events (seq bigint, row_hash text)`); err != nil {
		t.Fatalf("create shadowing temp table: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO pg_temp.audit_events VALUES (1, $1)`, forged); err != nil {
		t.Fatalf("seed shadowing temp table: %v", err)
	}
	// The definer reads as the function's OWNER; a real attacker holding only
	// INSERT+SELECT grants the owner access to their own temp table, so the
	// probe does the same rather than relying on being the owner already.
	var owner string
	if err := conn.QueryRow(ctx, `SELECT pg_get_userbyid(proowner) FROM pg_proc WHERE proname = 'audit_events_chain'`).Scan(&owner); err != nil {
		t.Fatalf("read chain function owner: %v", err)
	}
	if _, err := conn.Exec(ctx, `GRANT SELECT ON pg_temp.audit_events TO `+pgx.Identifier{owner}.Sanitize()); err != nil {
		t.Fatalf("grant on shadowing temp table: %v", err)
	}

	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(context.Background()) //nolint:errcheck
	// Hold the chain lock so the head cannot move under us; the trigger takes
	// the same (re-entrant) lock.
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, AuditChainLockKey); err != nil {
		t.Fatalf("advisory lock: %v", err)
	}
	var wantPrev string
	if err := tx.QueryRow(ctx, `SELECT COALESCE((SELECT row_hash FROM `+qualified+
		` WHERE row_hash IS NOT NULL ORDER BY seq DESC LIMIT 1), '')`).Scan(&wantPrev); err != nil {
		t.Fatalf("read the real chain head: %v", err)
	}
	if wantPrev == "" {
		t.Fatal("the real chain has no head after seeding it; the probe cannot distinguish a forged link from an empty chain")
	}

	var gotPrev string
	if err := tx.QueryRow(ctx, `INSERT INTO `+qualified+` (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'searchpath-probe', 'test.searchpath.tempshadow', 'success')
		RETURNING COALESCE(prev_hash, '')`).Scan(&gotPrev); err != nil {
		t.Fatalf("INSERT into %s with a shadowing temp table present: %v", qualified, err)
	}
	if gotPrev == forged {
		t.Fatalf("the chain trigger read its head from the CALLER's temp table: prev_hash = %q, the value the caller planted.\n"+
			"docs/OPERATIONS.md states prev_hash/row_hash are computed inside Postgres so no caller can choose them; a "+
			"SECURITY DEFINER search_path that does not end in pg_temp lets any INSERT-capable role choose both (F098)", gotPrev)
	}
	if gotPrev != wantPrev {
		t.Fatalf("prev_hash = %q, want the real preceding row's row_hash %q", gotPrev, wantPrev)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
}

// TestPG_ReplayingTheTriggerMigrationsIsIdempotent asserts the PROPERTY the
// text markers only stand in for: that every file in the boot-time replay set
// actually survives being re-executed against a database where it is already
// applied. The marker guard in db_test.go runs without Postgres and catches the
// common way to break this; only running the real statements catches the rest
// (a bare CREATE INDEX, a non-replaceable object, an ALTER that is not
// re-runnable).
//
// It calls replayTriggerMigrations directly, which is what ensureAuditTriggers
// does when it finds the chain trigger missing — and it does so TWICE, because
// a boot-restore that only works once is not a restore. Its own schema, so no
// lane's audit_events is dropped and re-created underneath it.
func TestPG_ReplayingTheTriggerMigrationsIsIdempotent(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	names, err := triggerMigrationFiles(auditChainTrigger)
	if err != nil {
		t.Fatalf("triggerMigrationFiles: %v", err)
	}
	t.Logf("replay set: %v", names)

	for pass := 1; pass <= 2; pass++ {
		if err := replayTriggerMigrations(ctx, pool, auditChainTrigger); err != nil {
			t.Fatalf("replay pass %d over %v failed: %v\n"+
				"the boot-time restore re-executes these files verbatim against a database where they are ALREADY "+
				"applied; a non-idempotent one turns the rescue path into a refused boot on exactly the database "+
				"whose audit trigger went missing", pass, names, err)
		}
	}

	// The replay must leave a WORKING chain, not merely exit zero: the trigger
	// attached, firing, and still linking rows.
	var firstHash string
	if err := pool.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'replay-probe', 'test.replay.1', 'success')
		RETURNING COALESCE(row_hash, '')`).Scan(&firstHash); err != nil {
		t.Fatalf("append after replay in %s: %v", schema, err)
	}
	if firstHash == "" {
		t.Fatal("a row appended after the replay carries no row_hash; the restored trigger is not chaining")
	}
	var secondPrev string
	if err := pool.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'replay-probe', 'test.replay.2', 'success')
		RETURNING COALESCE(prev_hash, '')`).Scan(&secondPrev); err != nil {
		t.Fatalf("second append after replay: %v", err)
	}
	if secondPrev != firstHash {
		t.Errorf("prev_hash after the replay is %q, want the preceding row's %q — the replay restored a trigger "+
			"that runs but does not link", secondPrev, firstHash)
	}
}
