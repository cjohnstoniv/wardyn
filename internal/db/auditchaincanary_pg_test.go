// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the review finding that the boot audit-trigger re-check was
// CATALOG-SHAPE-ONLY, so the exact failure class 0058 exists to repair — a
// trigger present, enabled and correctly named, but non-functional at runtime —
// passed boot silently.
//
// Every probe here leaves the catalog in the state the shape checks want: the
// trigger is attached, enabled, named audit_events_chain, and bound to a
// function named audit_events_chain in the table's own schema. Only the BODY is
// wrong — which is exactly what 0057 shipped, and exactly what no query about
// pg_trigger can see. The canary is the one round trip that can.
//
// Each subtest migrates into its OWN schema, so the lane's chain function is
// never replaced.

import (
	"context"
	"strings"
	"testing"
)

func TestPG_BootCanaryRefusesAChainTriggerThatDoesNotChain(t *testing.T) {
	ctx := context.Background()

	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			// The 0057 shape as an operator met it: the trigger runs, the row
			// lands, and nothing is hashed.
			name: "no row_hash at all",
			body: `BEGIN RETURN NEW; END;`,
			want: "NO row_hash",
		},
		{
			// The QUIETER variant 0058's header calls out: the head read
			// resolves somewhere else, so every row is a genesis row and the
			// chain never links.
			name: "prev_hash never links",
			body: `BEGIN NEW.row_hash := encode(sha256(NEW.id::text::bytea), 'hex'); NEW.prev_hash := NULL; RETURN NEW; END;`,
			want: "chain head is",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, schema := probeSchemaPool(t)

			// Seed a real head first, so "prev_hash never links" is a genuine
			// mismatch rather than the legitimate empty-chain case.
			if _, err := pool.Exec(ctx, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'system', 'canary-probe', 'test.canary.seed', '', 'success', '', NULL)`); err != nil {
				t.Fatalf("seed the chain in %s: %v", schema, err)
			}

			// Same NAME, same SCHEMA, same trigger row — only the body differs.
			if _, err := pool.Exec(ctx, `CREATE OR REPLACE FUNCTION `+schema+`.audit_events_chain() RETURNS trigger
				LANGUAGE plpgsql AS $$ `+tc.body+` $$`); err != nil {
				t.Fatalf("replace the chain body in %s: %v", schema, err)
			}

			// Precondition: every SHAPE check still reports healthy, so the
			// verdict below can only come from the canary.
			present, err := auditTriggerNames(ctx, pool)
			if err != nil {
				t.Fatalf("auditTriggerNames: %v", err)
			}
			if !present[auditChainTrigger] {
				t.Fatalf("precondition: the trigger is not catalogued as present; the probe is not testing a shape-clean failure")
			}
			impostors, err := auditImpostorTriggers(ctx, pool)
			if err != nil {
				t.Fatalf("auditImpostorTriggers: %v", err)
			}
			if fn := impostors[auditChainTrigger]; fn != "" {
				t.Fatalf("precondition: the trigger reads as an impostor (%s); the probe must leave the catalog shape clean", fn)
			}

			err = Migrate(ctx, pool)
			if err == nil {
				t.Fatalf("Migrate returned nil over a chain trigger that is present, enabled, correctly named, bound to the " +
					"correctly named function — and does not chain. That is the 0057 state 0058 exists to repair, and every " +
					"row this process wrote would be reported as a break by the verify sweep")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Migrate error = %q; it must say what the canary observed (%q)", err, tc.want)
			}
		})
	}
}

// TestPG_BootCanaryCommitsNothing is the other direction: the canary must not
// leave a synthetic row in anybody's audit log, and must not refuse a healthy
// boot. Seq gaps from the rolled-back insert are expected and are what
// store.auditChainWalk already documents as benign.
func TestPG_BootCanaryCommitsNothing(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	countRows := func() int {
		t.Helper()
		var n int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM audit_events`).Scan(&n); err != nil {
			t.Fatalf("count audit_events in %s: %v", schema, err)
		}
		return n
	}
	before := countRows()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate on a healthy schema: %v — the canary must not refuse a working chain", err)
	}
	if after := countRows(); after != before {
		t.Errorf("audit_events grew from %d to %d across Migrate; the canary row was COMMITTED", before, after)
	}
	if _, err := pool.Exec(ctx, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'system', 'canary-probe', 'test.canary.after', '', 'success', '', NULL)`); err != nil {
		t.Fatalf("append after the canary: %v", err)
	}
	var canaries int
	if err := pool.QueryRow(ctx,
		`SELECT count(*) FROM audit_events WHERE action = 'audit.chain.canary'`).Scan(&canaries); err != nil {
		t.Fatalf("look for canary rows: %v", err)
	}
	if canaries != 0 {
		t.Errorf("%d canary row(s) are in the audit log; the boot check must never commit one", canaries)
	}
}

// TestPG_BootCanarySurvivesARemovedTailRow: the chain links to the recorded head (0130), so a row
// removed from the newest end leaves the table's newest hash behind the head the trigger uses. The
// canary reads the head the way the trigger does, so it must still pass, and the next real append
// must still chain from the removed row's hash, which no retained row carries.
func TestPG_BootCanarySurvivesARemovedTailRow(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	var super bool
	if err := pool.QueryRow(ctx, `SELECT rolsuper FROM pg_roles WHERE rolname = current_user`).Scan(&super); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !super {
		t.Skip("WARDYN_TEST_PG role is not a superuser, so the tail row cannot be removed past the append-only triggers")
	}

	appendRow := func(action string) (id, rowHash, prevHash string) {
		t.Helper()
		if err := pool.QueryRow(ctx, `WITH i AS (SELECT gen_random_uuid() AS id)
			SELECT i.id::text, COALESCE(a.row_hash, ''), COALESCE(a.prev_hash, '')
			FROM i, audit_append(i.id, now(), NULL, 'system', 'canary-probe', $1, '', 'success', '', NULL) a`,
			action).Scan(&id, &rowHash, &prevHash); err != nil {
			t.Fatalf("append %s in %s: %v", action, schema, err)
		}
		return id, rowHash, prevHash
	}
	appendRow("test.canary.tail.1")
	tailID, tailHash, _ := appendRow("test.canary.tail.2")

	if err := AuditChainCanary(ctx, pool); err != nil {
		t.Fatalf("canary on an intact chain: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tamper tx: %v", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // best-effort on the failure path
	if _, err := tx.Exec(ctx, `SET LOCAL session_replication_role = replica`); err != nil {
		t.Skipf("no trigger bypass: %v", err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM audit_events WHERE id = $1::uuid`, tailID); err != nil {
		t.Fatalf("remove the newest row: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the removal: %v", err)
	}

	if err := AuditChainCanary(ctx, pool); err != nil {
		t.Fatalf("canary after a tail row was removed: %v — the head it reads must be the one the trigger links to", err)
	}

	_, _, prev := appendRow("test.canary.tail.after")
	if prev != tailHash {
		t.Errorf("the append after the removal chains to %q, want the removed row's hash %q", prev, tailHash)
	}
}
