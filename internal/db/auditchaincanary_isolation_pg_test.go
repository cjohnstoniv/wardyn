// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PIN for the follow-on gap in the boot canary: it started its transaction with
// a bare Begin, so it INHERITED default_transaction_isolation — the same USERSET
// GUC the audit writers were fixed to stop trusting one commit earlier.
//
// The canary reads the chain head and then INSERTs, and the trigger reads the
// head again inside that INSERT. At REPEATABLE READ both reads answer from a
// snapshot the advisory-lock statement took BEFORE the lock was granted, so the
// canary validates a world that may already be stale; at SERIALIZABLE the same
// transaction can be aborted with a serialization failure that auditChainCanary
// reports as a broken chain and REFUSES THE BOOT over. A boot-time integrity
// check must not have its snapshot semantics decided by a deployment setting.
//
// HOW THE PROBE SEES INSIDE A TRANSACTION THAT IS ALWAYS ROLLED BACK: an AFTER
// INSERT trigger on the probe schema's audit_events reads
// current_setting('transaction_isolation') and, when it is not read committed,
// bumps a SEQUENCE. nextval is NOT transactional, so the witness survives the
// canary's rollback — which is the only way to observe the level the real
// production path actually ran at. An AFTER trigger is chosen deliberately: it
// cannot alter the stored row, so auditForeignTriggers reports it and lets the
// boot continue (TestPG_BootAcceptsAForeignAfterInsertTrigger pins that), and
// the canary under test still runs.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// repeatableReadProbePool is probeSchemaPool with one thing added: every
// connection it opens defaults to REPEATABLE READ, the deployment posture
// reachable with nothing but `ALTER ROLE ... SET default_transaction_isolation`.
func repeatableReadProbePool(t *testing.T) (*pgxpool.Pool, string) {
	t.Helper()
	dsn := os.Getenv("WARDYN_TEST_PG")
	if dsn == "" {
		t.Skip("WARDYN_TEST_PG not set")
	}
	u, err := url.Parse(dsn)
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN; cannot point a connection at another schema")
	}
	ctx := context.Background()
	base := pgPool(t)
	schema := fmt.Sprintf("wardyn_ci_%d", time.Now().UnixNano()%1_000_000_000)
	if _, err := base.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create probe schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := base.Exec(context.Background(), `DROP SCHEMA IF EXISTS `+schema+` CASCADE`); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
	})
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		t.Fatalf("parse DSN: %v", err)
	}
	cfg.ConnConfig.RuntimeParams["search_path"] = schema
	cfg.ConnConfig.RuntimeParams["default_transaction_isolation"] = "repeatable read"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("connect with search_path=%s: %v", schema, err)
	}
	t.Cleanup(pool.Close)
	var iso string
	if err := pool.QueryRow(ctx, `SELECT current_setting('default_transaction_isolation')`).Scan(&iso); err != nil {
		t.Fatalf("read default_transaction_isolation: %v", err)
	}
	if iso != "repeatable read" {
		t.Skipf("default_transaction_isolation is %q, not repeatable read; this server does not accept it as a startup parameter", iso)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate into schema %s: %v", schema, err)
	}
	return pool, schema
}

func TestPG_BootCanaryRunsAtReadCommittedWhateverTheServerDefaultIs(t *testing.T) {
	pool, schema := repeatableReadProbePool(t)
	ctx := context.Background()

	for _, q := range []string{
		`CREATE SEQUENCE iso_witness`,
		`CREATE FUNCTION iso_witness_fn() RETURNS trigger LANGUAGE plpgsql AS $$
		 BEGIN
		   IF current_setting('transaction_isolation') <> 'read committed' THEN
		     PERFORM nextval('iso_witness');
		   END IF;
		   RETURN NULL;
		 END; $$`,
		// AFTER, so the boot check reports it rather than refusing over it.
		`CREATE TRIGGER audit_events_zz_iso_witness AFTER INSERT ON audit_events
		 FOR EACH ROW EXECUTE FUNCTION iso_witness_fn()`,
	} {
		if _, err := pool.Exec(ctx, q); err != nil {
			t.Fatalf("install the isolation witness in %s: %v", schema, err)
		}
	}

	witness := func() int64 {
		t.Helper()
		var last int64
		var called bool
		if err := pool.QueryRow(ctx, `SELECT last_value, is_called FROM iso_witness`).Scan(&last, &called); err != nil {
			t.Fatalf("read the witness: %v", err)
		}
		if !called {
			return 0
		}
		return last
	}

	// CONTROL: the witness fires for a transaction that DOES inherit the
	// server default, so a green result below cannot be a witness that never
	// works.
	before := witness()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin control tx: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'iso-witness', 'test.canary.control', 'success')`); err != nil {
		tx.Rollback(context.Background()) //nolint:errcheck
		t.Fatalf("control insert: %v", err)
	}
	if err := tx.Rollback(context.Background()); err != nil {
		t.Fatalf("rollback control tx: %v", err)
	}
	if witness() == before {
		t.Fatalf("the witness did not fire for a transaction that inherits repeatable read; the probe cannot see what it claims to")
	}

	// THE REAL PATH: Migrate runs auditChainCanary, which appends one row and
	// rolls it back. The witness must NOT move.
	mark := witness()
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate on a repeatable-read pool: %v", err)
	}
	if got := witness(); got != mark {
		t.Fatalf("the boot audit-chain canary ran at the server's default isolation (the witness advanced %d -> %d) "+
			"instead of pinning READ COMMITTED. Its head read and the trigger's head read then answer from a snapshot "+
			"taken before the chain lock was granted, so the canary validates a stale world - and at SERIALIZABLE the "+
			"same transaction can be aborted with a serialization failure that auditChainCanary reports as a broken "+
			"chain and REFUSES THE BOOT over. Pin the level at the transaction, as store.InsertAuditEvent does.",
			mark, got)
	}
}
