// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the boot-time audit-trigger restore replayed its four files as
// four independent Execs, so a failure partway through COMMITTED a superseded
// definition of the chain function and left it there.
//
// The replay set is 0047 → 0056 → 0057 → 0058, and 0047's body is the ORIGINAL,
// UNSERIALIZED chain function — the one 0056 replaced with a version that takes
// pg_advisory_xact_lock before it reads the chain head. Stop after 0047 and the
// database is left running 0047's function while every boot-time check passes:
// auditTriggerNames sees the name, auditImpostorTriggers sees the shipped
// function (it cannot see a replaced BODY), and the single-threaded canary
// chains fine. Any concurrent writer then forks the chain, and
// GET /audit/chain/verify reports a tamper verdict no operator can clear.
//
// The exits that stop it partway are ordinary: migrateOn runs under wardynd's
// connect-and-migrate deadline, and an expiring context mid-loop is a
// designed-for exit (migrate_hardening_cancel_pg_test.go). applyMigration
// already wraps each FORWARD migration in a transaction; this path did not, and
// unlike the forward path it never self-heals — the entry condition (the trigger
// missing, or wearing an impostor body) is satisfied by 0047 alone, so the next
// boot finds a present, shipped-looking trigger and replays nothing.
//
// Needs a live Postgres (WARDYN_TEST_PG); skips cleanly without one.

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errReplayFileFailed is what the executor below raises in place of the second
// replayed file — the shape of any mid-replay failure (a dead context, a lost
// connection, a statement the server refuses).
var errReplayFileFailed = errors.New("probe: the second replayed migration failed")

// failAtReplayedFile is a migrationExecutor that lets the FIRST replayed
// migration through and fails the second, whether the replay runs its files
// straight on the connection or inside one transaction. Both arms share the
// counter, so the probe drives the same cut either way and does not silently
// stop testing anything the day the replay grows a transaction.
type failAtReplayedFile struct {
	migrationExecutor
	at   int // 1-based index of the replayed file to fail
	seen int
}

func (f *failAtReplayedFile) replayed(sql string) bool {
	return strings.Contains(sql, "TRIGGER "+auditChainTrigger)
}

func (f *failAtReplayedFile) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if f.replayed(sql) {
		f.seen++
		if f.seen == f.at {
			return pgconn.CommandTag{}, errReplayFileFailed
		}
	}
	return f.migrationExecutor.Exec(ctx, sql, args...)
}

func (f *failAtReplayedFile) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := f.migrationExecutor.Begin(ctx)
	if err != nil {
		return nil, err
	}
	return &failAtReplayedTx{Tx: tx, owner: f}, nil
}

type failAtReplayedTx struct {
	pgx.Tx
	owner *failAtReplayedFile
}

func (t *failAtReplayedTx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if t.owner.replayed(sql) {
		t.owner.seen++
		if t.owner.seen == t.owner.at {
			return pgconn.CommandTag{}, errReplayFileFailed
		}
	}
	return t.Tx.Exec(ctx, sql, args...)
}

// chainFunctionDef is the chain function's body as the catalog holds it.
func chainFunctionDef(t *testing.T, pool *pgxpool.Pool) string {
	t.Helper()
	var def string
	if err := pool.QueryRow(context.Background(),
		`SELECT pg_get_functiondef(p.oid)
		   FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
		  WHERE p.proname = $1 AND n.nspname = current_schema()`, auditChainTrigger).Scan(&def); err != nil {
		t.Fatalf("read %s()'s definition: %v", auditChainTrigger, err)
	}
	return def
}

// serializedChainDef is the marker that tells 0056+'s chain function apart from
// 0047's: the in-trigger advisory lock that makes two concurrent inserters
// serialize on the chain head instead of both reading it.
const serializedChainDef = "pg_advisory_xact_lock"

func TestPG_AFailedTriggerReplayLeavesNoSupersededChainFunction(t *testing.T) {
	pool, schema := probeSchemaPool(t)
	ctx := context.Background()

	if def := chainFunctionDef(t, pool); !strings.Contains(def, serializedChainDef) {
		t.Fatalf("precondition: the freshly migrated schema %s is not running the serialized chain function; "+
			"the probe cannot tell a rollback from the state it started in", schema)
	}
	names, err := triggerMigrationFiles(auditChainTrigger)
	if err != nil {
		t.Fatalf("triggerMigrationFiles: %v", err)
	}
	if len(names) < 2 {
		t.Skipf("the replay set is %v; this probe needs at least two files to cut between them", names)
	}

	// THE OPERATOR'S HALF of the entry condition: the trigger is gone, which is
	// what sends ensureAuditTriggers into the replay in the first place.
	if _, err := pool.Exec(ctx, `DROP TRIGGER `+auditChainTrigger+` ON audit_events`); err != nil {
		t.Fatalf("drop the chain trigger: %v", err)
	}

	ex := &failAtReplayedFile{migrationExecutor: pool, at: 2}
	err = replayTriggerMigrations(ctx, ex, auditChainTrigger)
	if err == nil {
		t.Fatalf("replayTriggerMigrations returned nil though %s was made to fail; the probe did not reproduce a "+
			"partial replay", names[1])
	}
	if !errors.Is(err, errReplayFileFailed) {
		t.Fatalf("the replay failed with %v, not the injected failure — the probe cut somewhere it did not mean to", err)
	}

	if def := chainFunctionDef(t, pool); !strings.Contains(def, serializedChainDef) {
		t.Fatalf("after a replay that failed at %s, %s() is bound to a SUPERSEDED body (no %s).\n"+
			"replay set: %v, schema: %s.\n"+
			"0047's function reads the chain head with no advisory lock, so two concurrent inserters both read the "+
			"same head and fork the chain — and nothing reports it: the trigger's NAME is present, its FUNCTION is "+
			"the one Wardyn ships, and the single-threaded boot canary passes. The next boot replays nothing, so "+
			"this does not self-heal; GET /audit/chain/verify reports a tamper verdict no operator can clear.",
			names[1], auditChainTrigger, serializedChainDef, names, schema)
	}

	// And the retry — the operator's actual next move — still restores. A replay
	// that rolled back must leave a database the next boot can fix.
	if err := replayTriggerMigrations(ctx, pool, auditChainTrigger); err != nil {
		t.Fatalf("the retried replay failed: %v", err)
	}
	present, err := auditTriggerNames(ctx, pool)
	if err != nil {
		t.Fatalf("auditTriggerNames after the retry: %v", err)
	}
	if !present[auditChainTrigger] {
		t.Fatal("the retried replay did not put the chain trigger back")
	}
	if def := chainFunctionDef(t, pool); !strings.Contains(def, serializedChainDef) {
		t.Errorf("after the retried replay %s() is still not the serialized definition", auditChainTrigger)
	}
}
