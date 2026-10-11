// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// Live tests for the partitioned audit log (0111 and 0112): the conversion of a populated 0.8.5
// chain, the append protocol and its guards, the privilege model, and the replayable trigger
// definition. Every one needs WARDYN_TEST_PG and a role that can CREATE ROLE (the split-role posture
// is the point), and every one runs against whatever Postgres version that variable names: CI runs
// the package on 13 (the floor) and on 17.

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const auditAppendFn = "audit_append(uuid, timestamptz, uuid, text, text, text, text, text, text, jsonb)"

// pgExec runs one statement and fails the test on error.
func pgExec(t *testing.T, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(context.Background(), sql, args...); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
}

// pgScalar reads one value.
func pgScalar[T any](t *testing.T, pool *pgxpool.Pool, sql string, args ...any) T {
	t.Helper()
	var v T
	if err := pool.QueryRow(context.Background(), sql, args...).Scan(&v); err != nil {
		t.Fatalf("%s: %v", strings.Join(strings.Fields(sql), " "), err)
	}
	return v
}

// appendAudit appends one row through audit_append on pool and returns (seq, prev_hash, row_hash).
func appendAudit(t *testing.T, pool *pgxpool.Pool, action string) (int64, string, string) {
	t.Helper()
	var seq int64
	var prev, hash string
	if err := pool.QueryRow(context.Background(), `
		SELECT seq, COALESCE(prev_hash, ''), COALESCE(row_hash, '')
		  FROM audit_append(gen_random_uuid(), now(), NULL, 'system', 'ar-l1.1', $1, '', 'success', '', NULL)`,
		action).Scan(&seq, &prev, &hash); err != nil {
		t.Fatalf("audit_append %s: %v", action, err)
	}
	return seq, prev, hash
}

// chainBreaks counts the rows whose stored hash does not match a recomputation, or whose prev_hash is
// not the preceding hashed row's row_hash: the same two rules VerifyAuditChain applies, in SQL, so the
// db package can check a chain without importing the store.
func chainBreaks(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	return pgScalar[int](t, pool, `
		SELECT count(*)::int FROM (
			SELECT prev_hash, row_hash, lag(row_hash) OVER (ORDER BY seq) AS expect_prev,
			       audit_row_hash(prev_hash, id, "time", run_id, actor_type, actor, action, target, outcome, source_ip, data) AS calc
			  FROM audit_events WHERE row_hash IS NOT NULL) x
		 WHERE calc IS DISTINCT FROM row_hash OR (expect_prev IS NOT NULL AND prev_hash IS DISTINCT FROM expect_prev)`)
}

// auditRole creates a login role, dropped (with everything it was granted) on cleanup.
func auditRole(t *testing.T, base *pgxpool.Pool, prefix string) (role, password string) {
	t.Helper()
	var canCreate bool
	if err := base.QueryRow(context.Background(),
		`SELECT rolsuper OR rolcreaterole FROM pg_roles WHERE rolname = current_user`).Scan(&canCreate); err != nil {
		t.Fatalf("read role: %v", err)
	}
	if !canCreate {
		t.Skip("WARDYN_TEST_PG role cannot CREATE ROLE; the split-role audit tests need it")
	}
	role = fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()%1_000_000_000)
	password = "ar-l1-1-pw"
	pgExec(t, base, fmt.Sprintf(`CREATE ROLE %s LOGIN PASSWORD '%s'`, role, password))
	t.Cleanup(func() {
		ctx := context.Background()
		_, _ = base.Exec(ctx, `DROP OWNED BY `+role)
		if _, err := base.Exec(ctx, `DROP ROLE IF EXISTS `+role); err != nil {
			t.Logf("cleanup drop role %s: %v", role, err)
		}
	})
	return role, password
}

// poolAs connects to the same database and schema as owner, as role.
func poolAs(t *testing.T, schema, role, password string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN")
	}
	u.User = url.UserPassword(role, password)
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := Connect(context.Background(), u.String())
	if err != nil {
		t.Skipf("cannot connect as %s (pg_hba may not password-auth a new role): %v", role, err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// upgradeFixture is a 0.8.5-shaped database (every migration before 0111, which includes 0107's
// floor): audit_events is an ordinary table whose chain was written by the OLD trigger, with the
// documented split-role posture (INSERT and SELECT for an app role whose name is deliberately not
// wardyn_app). The conversion has NOT run.
type upgradeFixture struct {
	owner   *pgxpool.Pool
	schema  string
	appRole string
	appPass string
	rows    int
	bulk    int // WARDYN_TEST_AUDIT_UPGRADE_ROWS
}

func newUpgradeFixture(t *testing.T, rows int) *upgradeFixture {
	t.Helper()
	owner, schema := partialSchemaPool(t, auditConversionFile)
	f := &upgradeFixture{owner: owner, schema: schema, rows: rows}
	f.appRole, f.appPass = auditRole(t, pgPool(t), "wardyn_ar_app")
	pgExec(t, owner, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, schema, f.appRole))
	pgExec(t, owner, `GRANT SELECT, INSERT ON audit_events TO `+f.appRole)
	// WARDYN_TEST_AUDIT_UPGRADE_ROWS adds that many unchained pre-0047-style rows ahead of the chain, in
	// bulk, so the conversion's cost can be measured at a realistic table size; unset, the fixture is small.
	if n, _ := strconv.Atoi(os.Getenv("WARDYN_TEST_AUDIT_UPGRADE_ROWS")); n > 0 {
		pgExec(t, owner, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_chain`)
		pgExec(t, owner, `INSERT INTO audit_events (id, time, run_id, actor_type, actor, action, target, outcome, source_ip, data)
			SELECT gen_random_uuid(), now() - (g || ' seconds')::interval, gen_random_uuid(), 'human', 'user' || (g % 100) || '@example.com',
			       'run.event.' || (g % 40), 'target-' || g, 'success', '10.0.0.' || (g % 250), jsonb_build_object('n', g, 'k', 'v')
			  FROM generate_series(1, $1::int) g`, n)
		pgExec(t, owner, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_chain`)
		pgExec(t, owner, `ANALYZE audit_events`)
		f.bulk = n
	}
	// The 0.8.5 write path: a direct INSERT, chained by the trigger.
	pgExec(t, owner, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		SELECT gen_random_uuid(), 'system', 'seed', 'seed.' || g, 'success' FROM generate_series(1, $1::int) g`, rows)
	if got := pgScalar[string](t, owner, `SELECT relkind::text FROM pg_class WHERE oid = 'audit_events'::regclass`); got != "r" {
		t.Fatalf("fixture: audit_events relkind = %q before the conversion, want an ordinary table", got)
	}
	return f
}

// convert runs Migrate (0111, 0112) and returns how long it took.
func (f *upgradeFixture) convert(t *testing.T) time.Duration {
	t.Helper()
	start := time.Now()
	if err := Migrate(context.Background(), f.owner); err != nil {
		t.Fatalf("Migrate over the 0.8.5 schema: %v", err)
	}
	return time.Since(start)
}

func (f *upgradeFixture) app(t *testing.T) *pgxpool.Pool {
	return poolAs(t, f.schema, f.appRole, f.appPass)
}

// TestPG_AuditPartition_ConvertsAPopulated085Chain is the upgrade test: a seeded 0.8.5 chain goes
// through 0111 and 0112 and comes out verifying, with its grants, its sequence and its indexes.
func TestPG_AuditPartition_ConvertsAPopulated085Chain(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 40)
	maxSeqBefore := pgScalar[int64](t, f.owner, `SELECT max(seq) FROM audit_events`)
	headBefore := pgScalar[string](t, f.owner, `SELECT row_hash FROM audit_events ORDER BY seq DESC LIMIT 1`)
	if chainBreaks(t, f.owner) != 0 {
		t.Fatal("fixture: the 0.8.5 chain is already broken before the conversion")
	}

	elapsed := f.convert(t)
	t.Logf("conversion of %d rows took %s (postgres %s)", f.rows+f.bulk, elapsed,
		pgScalar[string](t, f.owner, `SHOW server_version`))

	if got := pgScalar[string](t, f.owner, `SELECT relkind::text FROM pg_class WHERE oid = 'audit_events'::regclass`); got != "p" {
		t.Fatalf("audit_events relkind = %q after the conversion, want a partitioned table", got)
	}
	if got := pgScalar[int](t, f.owner, `SELECT count(*)::int FROM audit_events`); got != f.rows+f.bulk {
		t.Errorf("rows after the conversion = %d, want the %d that were there", got, f.rows+f.bulk)
	}
	if got := pgScalar[int](t, f.owner, `SELECT count(*)::int FROM audit_events_legacy`); got != f.rows+f.bulk {
		t.Errorf("legacy partition holds %d rows, want all %d of the pre-0.8.6 history", got, f.rows+f.bulk)
	}
	if got := chainBreaks(t, f.owner); got != 0 {
		t.Errorf("%d chain break(s) after the conversion; the rows verify no more", got)
	}

	// Grants copied: the app role keeps SELECT, loses INSERT everywhere, and gains EXECUTE.
	privs := func(role string) (sel, ins, legacyIns, exec bool) {
		if err := f.owner.QueryRow(ctx, `SELECT has_table_privilege($1, 'audit_events', 'SELECT'),
			has_table_privilege($1, 'audit_events', 'INSERT'),
			has_table_privilege($1, 'audit_events_legacy', 'INSERT'),
			has_function_privilege($1, to_regprocedure('`+auditAppendFn+`'), 'EXECUTE')`, role).Scan(&sel, &ins, &legacyIns, &exec); err != nil {
			t.Fatalf("read privileges of %s: %v", role, err)
		}
		return
	}
	if sel, ins, legacyIns, exec := privs(f.appRole); !sel || ins || legacyIns || !exec {
		t.Errorf("app role %s after the conversion: SELECT=%v INSERT=%v legacy INSERT=%v EXECUTE audit_append=%v, "+
			"want true false false true", f.appRole, sel, ins, legacyIns, exec)
	}

	// seq continues, the chain links across the boundary, and the new row is not in the legacy partition.
	app := f.app(t)
	seq, prev, _ := appendAudit(t, app, "test.after.conversion")
	if seq <= maxSeqBefore {
		t.Errorf("first seq after the conversion = %d, want above the old head %d", seq, maxSeqBefore)
	}
	if prev != headBefore {
		t.Errorf("first row after the conversion chains to %q, want the 0.8.5 head %q", prev, headBefore)
	}
	if rel := pgScalar[string](t, f.owner, `SELECT tableoid::regclass::text FROM audit_events WHERE seq = $1`, seq); strings.Contains(rel, "legacy") {
		t.Errorf("the first live row went into %s", rel)
	}
	if got := chainBreaks(t, f.owner); got != 0 {
		t.Errorf("%d chain break(s) after the first live append", got)
	}

	// Indexes: the parent has the full set, and a partition created by audit_ensure_partitions has its own.
	wantIdx := []string{"audit_events_action_seq_idx", "audit_events_chain_head_idx", "audit_events_device_origin_idx",
		"audit_events_pkey", "audit_events_run_idx", "audit_events_run_seq_idx", "audit_events_time_idx"}
	parentIdx := auditIndexNames(t, f.owner, "audit_events")
	if !slices.Equal(parentIdx, wantIdx) {
		t.Errorf("parent indexes = %v, want %v", parentIdx, wantIdx)
	}
	fresh := pgScalar[string](t, f.owner, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m ORDER BY m->>'hi' DESC LIMIT 1`)
	if got, want := len(auditIndexNames(t, f.owner, fresh)), len(wantIdx); got != want {
		t.Errorf("fresh partition %s has %d indexes, want %d (one per parent index)", fresh, got, want)
	}

	// The serving role's view of its own privileges, which is what boot reports on.
	ap, err := AuditAppendPostureOf(ctx, app)
	if err != nil {
		t.Fatalf("AuditAppendPostureOf(app): %v", err)
	}
	if !ap.CanAppend || ap.DirectInsert || len(ap.PublicExecute) != 0 || ap.Role != f.appRole {
		t.Errorf("app role posture = %+v, want CanAppend and nothing else", ap)
	}
	op, err := AuditAppendPostureOf(ctx, f.owner)
	if err != nil {
		t.Fatalf("AuditAppendPostureOf(owner): %v", err)
	}
	if !op.CanAppend || !op.DirectInsert {
		t.Errorf("owner posture = %+v, want CanAppend and DirectInsert (the single-role case)", op)
	}
}

// TestPG_AuditPartition_ConversionAfterARolledBackInsertVerifies: a 0.8.5 insert that rolled back
// burned an identity value no committed row holds. The conversion must record the newest COMMITTED
// row as the high-water mark (verify compares the head against it, so a burned value reads as a
// truncated tail), and must still start the new sequence above the burned value.
func TestPG_AuditPartition_ConversionAfterARolledBackInsertVerifies(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 5)
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	var burned int64
	if err := tx.QueryRow(ctx, `INSERT INTO audit_events (id, actor_type, actor, action, outcome)
		VALUES (gen_random_uuid(), 'system', 'seed', 'seed.rolled.back', 'success') RETURNING seq`).Scan(&burned); err != nil {
		t.Fatalf("burn a seq: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	maxSeq := pgScalar[int64](t, f.owner, `SELECT max(seq) FROM audit_events`)
	head := pgScalar[string](t, f.owner, `SELECT row_hash FROM audit_events ORDER BY seq DESC LIMIT 1`)
	if burned <= maxSeq {
		t.Fatalf("fixture: burned seq %d is not above the committed head %d", burned, maxSeq)
	}

	f.convert(t)

	// The two facts verifyAuditPartitionState compares, before any new append.
	hwSeq := pgScalar[int64](t, f.owner, `SELECT hw_seq FROM audit_partition_meta`)
	hwHash := pgScalar[string](t, f.owner, `SELECT COALESCE(hw_row_hash, '') FROM audit_partition_meta`)
	if hwSeq != maxSeq || hwHash != head {
		t.Errorf("high-water mark after the conversion = (seq %d, hash %q), want the committed head (seq %d, hash %q)",
			hwSeq, hwHash, maxSeq, head)
	}
	if got := chainBreaks(t, f.owner); got != 0 {
		t.Errorf("%d chain break(s) after the conversion", got)
	}

	seq, prev, _ := appendAudit(t, f.app(t), "test.after.rollback")
	if seq <= burned {
		t.Errorf("first seq after the conversion = %d, want above the burned %d (no reuse)", seq, burned)
	}
	if prev != head {
		t.Errorf("first row after the conversion chains to %q, want the committed head %q", prev, head)
	}
}

func auditIndexNames(t *testing.T, pool *pgxpool.Pool, table string) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND tablename = $1 ORDER BY 1`, table)
	if err != nil {
		t.Fatalf("list indexes of %s: %v", table, err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		// A partition's own index names embed the partition's name; compare by suffix class.
		if table != "audit_events" {
			n = "audit_events_" + strings.TrimPrefix(n, table+"_")
		}
		names = append(names, n)
	}
	return names
}

// TestPG_AuditPartition_GuardsRefuseWhatAuditAppendDidNotAllocate drives the refusals from a
// connection that is NOT the one that migrated.
func TestPG_AuditPartition_GuardsRefuseWhatAuditAppendDidNotAllocate(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 3)
	f.convert(t)
	app := f.app(t)

	t.Run("a forged recorded_at is refused", func(t *testing.T) {
		appendAudit(t, f.owner, "test.hw.seed") // the high-water mark is now a live row, above the cutover
		for name, c := range map[string]struct{ at, want string }{
			"control: the server clock is accepted": {`clock_timestamp()`, ""},
			"ahead of the clock":                    {`now() + interval '1 day'`, "ahead of the server clock"},
			"behind the high-water":                 {`(SELECT hw_recorded_at - interval '1 microsecond' FROM audit_partition_meta)`, "before the chain high-water mark"},
			"before the cutover":                    {`'-infinity'::timestamptz + interval '1 day'`, "before the chain high-water mark"},
		} {
			at := c.at
			tx, err := f.owner.Begin(ctx)
			if err != nil {
				t.Fatalf("begin: %v", err)
			}
			var next int64
			if err := tx.QueryRow(ctx, `SELECT nextval('audit_events_seq')`).Scan(&next); err != nil {
				t.Fatalf("nextval: %v", err)
			}
			if _, err := tx.Exec(ctx, `SELECT set_config('wardyn.audit_seq', $1, true)`, fmt.Sprint(next)); err != nil {
				t.Fatalf("set_config: %v", err)
			}
			_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO audit_events (seq, id, actor_type, actor, action, outcome, recorded_at)
				VALUES (%d, gen_random_uuid(), 'system', 'forger', 'test.forged', 'success', %s)`, next, at))
			tx.Rollback(ctx) //nolint:errcheck
			switch {
			case c.want == "" && err != nil:
				t.Errorf("%s: refused: %v (the probe's own preconditions are wrong)", name, err)
			case c.want != "" && err == nil:
				t.Errorf("%s: a forged recorded_at was accepted", name)
			case c.want != "" && !strings.Contains(err.Error(), c.want):
				t.Errorf("%s: refused for the wrong reason: %v", name, err)
			}
		}
	})

	t.Run("a partition TRUNCATE is refused, and so is the parent's and the legacy one", func(t *testing.T) {
		live := pgScalar[string](t, f.owner, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m WHERE m->>'name' <> 'audit_events_legacy' ORDER BY m->>'lo' LIMIT 1`)
		for _, target := range []string{"audit_events", live, "audit_events_legacy"} {
			if _, err := f.owner.Exec(ctx, `TRUNCATE `+target); err == nil {
				t.Errorf("TRUNCATE %s succeeded; the append-only guard is not armed on it", target)
			}
		}
		if got := pgScalar[int](t, f.owner, `SELECT count(*)::int FROM audit_events`); got != 4 {
			t.Errorf("rows after the refused TRUNCATEs = %d, want the 3 seeded plus the 1 control append", got)
		}
	})

	t.Run("a multi-row INSERT is refused after its first row", func(t *testing.T) {
		tx, err := f.owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		var next int64
		if err := tx.QueryRow(ctx, `SELECT nextval('audit_events_seq')`).Scan(&next); err != nil {
			t.Fatalf("nextval: %v", err)
		}
		if _, err := tx.Exec(ctx, `SELECT set_config('wardyn.audit_seq', $1, true)`, fmt.Sprint(next)); err != nil {
			t.Fatalf("set_config: %v", err)
		}
		// Control: one row under the allocation is accepted.
		if _, err := tx.Exec(ctx, fmt.Sprintf(`INSERT INTO audit_events (seq, id, actor_type, actor, action, outcome)
			VALUES (%d, gen_random_uuid(), 'system', 'multi', 'test.multi.control', 'success')`, next)); err != nil {
			t.Fatalf("control: a single row under a fresh allocation was refused: %v", err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
		tx, err = f.owner.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck
		if _, err := tx.Exec(ctx, `SELECT set_config('wardyn.audit_seq', $1, true)`, fmt.Sprint(next)); err != nil {
			t.Fatalf("set_config: %v", err)
		}
		// The two rows share the one allocation the setting names. The first is the allocated position;
		// the second must be refused because the trigger consumed it.
		_, err = tx.Exec(ctx, fmt.Sprintf(`INSERT INTO audit_events (seq, id, actor_type, actor, action, outcome) VALUES
			(%[1]d, gen_random_uuid(), 'system', 'multi', 'test.multi.1', 'success'),
			(%[1]d, gen_random_uuid(), 'system', 'multi', 'test.multi.2', 'success')`, next))
		if err == nil {
			t.Fatal("a two-row INSERT under one allocation was accepted")
		}
		if !strings.Contains(err.Error(), "audit_append") {
			t.Errorf("multi-row refusal = %v, want the audit_append refusal", err)
		}
	})

	t.Run("a role holding only CONNECT cannot call audit_append", func(t *testing.T) {
		role, pw := auditRole(t, pgPool(t), "wardyn_ar_conn")
		pgExec(t, f.owner, fmt.Sprintf(`GRANT USAGE ON SCHEMA %s TO %s`, f.schema, role))
		conn := poolAs(t, f.schema, role, pw)
		_, err := conn.Exec(ctx, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'system', 'x', 'test.denied', '', 'success', '', NULL)`)
		if err == nil || !strings.Contains(err.Error(), "permission denied for function") {
			t.Errorf("CONNECT-only audit_append = %v, want permission denied for function", err)
		}
		p, perr := AuditAppendPostureOf(ctx, conn)
		if perr != nil {
			t.Fatalf("AuditAppendPostureOf: %v", perr)
		}
		if p.CanAppend {
			t.Errorf("a CONNECT-only role is reported able to append: %+v", p)
		}
	})

	t.Run("audit_ensure_partitions(1000) is refused as the app role and creates nothing", func(t *testing.T) {
		before := pgScalar[int](t, f.owner, `SELECT count(*)::int FROM pg_inherits WHERE inhparent = 'audit_events'::regclass`)
		for _, n := range []int{1000, 0, -1, 25} {
			if _, err := app.Exec(ctx, `SELECT audit_ensure_partitions($1)`, n); err == nil {
				t.Errorf("audit_ensure_partitions(%d) was accepted", n)
			}
		}
		if after := pgScalar[int](t, f.owner, `SELECT count(*)::int FROM pg_inherits WHERE inhparent = 'audit_events'::regclass`); after != before {
			t.Errorf("partitions went from %d to %d across refused calls", before, after)
		}
		// 12 is the one value callers pass, and the boot already created that many.
		if got := pgScalar[int](t, app, `SELECT audit_ensure_partitions(12)`); got != 0 {
			t.Errorf("audit_ensure_partitions(12) created %d partitions right after the boot created them", got)
		}
	})

	t.Run("the app role cannot write the log, the meta row or the anchors", func(t *testing.T) {
		for _, q := range []string{
			`INSERT INTO audit_events (seq, id, actor_type, actor, action, outcome) VALUES (1, gen_random_uuid(), 'system', 'x', 'x', 'success')`,
			`UPDATE audit_partition_meta SET hw_seq = 0`,
			`INSERT INTO audit_chain_anchors (kind, partition_name) VALUES ('drop', 'x')`,
			`DELETE FROM audit_chain_anchors`,
			`TRUNCATE audit_chain_anchors`,
		} {
			if _, err := app.Exec(ctx, q); err == nil {
				t.Errorf("the app role ran %s", q)
			}
		}
		if got := pgScalar[int](t, app, `SELECT count(*)::int FROM audit_partition_meta`); got != 1 {
			t.Errorf("the app role reads %d meta rows, want 1 (it may read the high-water mark)", got)
		}
	})
}

// TestPG_AuditPartition_NewPartitionsGrantNoWriteUnderDefaultPrivileges: the documented
// ALTER DEFAULT PRIVILEGES recipe hands the app role every future table, and a month created later by
// audit_ensure_partitions must not carry that into INSERT, UPDATE or DELETE on the log.
func TestPG_AuditPartition_NewPartitionsGrantNoWriteUnderDefaultPrivileges(t *testing.T) {
	f := newUpgradeFixture(t, 1)
	f.convert(t)
	pgExec(t, f.owner, fmt.Sprintf(`ALTER DEFAULT PRIVILEGES IN SCHEMA %s GRANT SELECT, INSERT, UPDATE, DELETE, TRUNCATE ON TABLES TO %s`, f.schema, f.appRole))
	pgExec(t, f.owner, `SELECT audit_ensure_partitions(24)`)
	newest := pgScalar[string](t, f.owner, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m ORDER BY m->>'hi' DESC LIMIT 1`)
	for _, priv := range []string{"INSERT", "UPDATE", "DELETE", "TRUNCATE", "TRIGGER", "REFERENCES"} {
		if pgScalar[bool](t, f.owner, `SELECT has_table_privilege($1, $2, $3)`, f.appRole, newest, priv) {
			t.Errorf("the app role holds %s on %s, created under default privileges that grant it", priv, newest)
		}
	}
	if !pgScalar[bool](t, f.owner, `SELECT has_table_privilege($1, $2, 'SELECT')`, f.appRole, newest) {
		t.Errorf("the app role cannot SELECT %s", newest)
	}
}

// TestPG_AuditPartition_EveryPartitionIsAContiguousPrefixOfChainOrder drives the appends the design
// names as the hazard: concurrent writers, a transaction that began before a month boundary and takes
// the chain lock after it, replayed events carrying an old time, and multi-row (multi-call)
// transactions. The month boundary is moved to a few seconds from now so it really is crossed.
func TestPG_AuditPartition_EveryPartitionIsAContiguousPrefixOfChainOrder(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 5)
	f.convert(t)
	app := f.app(t)

	// Re-cut the first live partition at "now + 4s", so a boundary falls inside the test.
	pgExec(t, f.owner, `
DO $$
DECLARE
    c timestamptz; e timestamptz; b timestamptz := clock_timestamp() + interval '4 seconds';
    nm text;
BEGIN
    SELECT m->>'name', (m->>'lo')::timestamptz, (m->>'hi')::timestamptz INTO nm, c, e
      FROM audit_partition_meta, jsonb_array_elements(manifest) m WHERE m->>'name' <> 'audit_events_legacy' ORDER BY m->>'lo' LIMIT 1;
    IF b >= e THEN RAISE EXCEPTION 'too close to a month end to place a boundary'; END IF;
    EXECUTE format('ALTER TABLE audit_events DETACH PARTITION %I', nm);
    EXECUTE format('DROP TABLE %I', nm);
    EXECUTE format('CREATE TABLE ar_p_a (LIKE audit_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)');
    EXECUTE format('CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON ar_p_a FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only()');
    EXECUTE format('ALTER TABLE audit_events ATTACH PARTITION ar_p_a FOR VALUES FROM (%L) TO (%L)', c, b);
    EXECUTE format('CREATE TABLE ar_p_b (LIKE audit_events INCLUDING DEFAULTS INCLUDING CONSTRAINTS)');
    EXECUTE format('CREATE TRIGGER audit_events_no_truncate BEFORE TRUNCATE ON ar_p_b FOR EACH STATEMENT EXECUTE FUNCTION audit_events_append_only()');
    EXECUTE format('ALTER TABLE audit_events ATTACH PARTITION ar_p_b FOR VALUES FROM (%L) TO (%L)', b, e);
    UPDATE audit_partition_meta SET manifest = (
        SELECT jsonb_build_array(
                 jsonb_build_object('name', 'ar_p_a', 'lo', to_char(c AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), 'hi', to_char(b AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')),
                 jsonb_build_object('name', 'ar_p_b', 'lo', to_char(b AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"'), 'hi', to_char(e AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')))
        || coalesce((SELECT jsonb_agg(x) FROM jsonb_array_elements(manifest) x WHERE x->>'name' <> nm AND x->>'lo' > to_char(c AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.US"Z"')), '[]'::jsonb));
END
$$`)
	boundary := pgScalar[time.Time](t, f.owner, `SELECT (m->>'hi')::timestamptz FROM audit_partition_meta, jsonb_array_elements(manifest) m WHERE m->>'name' = 'ar_p_a'`)
	stop := boundary.Add(1500 * time.Millisecond)

	var wg sync.WaitGroup
	errs := make(chan error, 64)
	worker := func(id int, replay, batch bool) {
		defer wg.Done()
		for n := 0; time.Now().Before(stop); n++ {
			tx, err := app.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			calls := 1
			if batch {
				calls = 4
			}
			for i := 0; i < calls; i++ {
				at := "now()"
				if replay {
					at = "now() - interval '40 days'" // a spooled event keeps its ORIGINAL time
				}
				if _, err := tx.Exec(ctx, fmt.Sprintf(`SELECT audit_append(gen_random_uuid(), %s, NULL, 'system', 'w%d', 'test.contiguous', '', 'success', '', NULL)`, at, id)); err != nil {
					tx.Rollback(ctx) //nolint:errcheck
					errs <- err
					return
				}
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
				return
			}
			time.Sleep(time.Duration(5+id) * time.Millisecond)
		}
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go worker(i, false, false)
	}
	wg.Add(2)
	go worker(10, true, false) // spool replay: old time, interleaved with live appends
	go worker(11, false, true) // multi-call transactions

	// Delayed transactions: they BEGIN before the boundary, and take the chain lock after it.
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			tx, err := app.Begin(ctx)
			if err != nil {
				errs <- err
				return
			}
			if _, err := tx.Exec(ctx, `SELECT now()`); err != nil { // pins transaction_timestamp() before the boundary
				errs <- err
				return
			}
			time.Sleep(time.Until(boundary) + time.Duration(200*(i+1))*time.Millisecond)
			if _, err := tx.Exec(ctx, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'system', 'delayed', 'test.delayed', '', 'success', '', NULL)`); err != nil {
				tx.Rollback(ctx) //nolint:errcheck
				errs <- err
				return
			}
			if err := tx.Commit(ctx); err != nil {
				errs <- err
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("a writer failed: %v", err)
	}

	type row struct {
		seq  int64
		part string
		at   time.Time
	}
	rows, err := f.owner.Query(ctx, `SELECT seq, tableoid::regclass::text, recorded_at FROM audit_events WHERE seq > (SELECT hw_seq FROM audit_partition_meta) - 100000 AND tableoid::regclass::text <> 'audit_events_legacy' ORDER BY seq`)
	if err != nil {
		t.Fatalf("read rows: %v", err)
	}
	defer rows.Close()
	var got []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.seq, &r.part, &r.at); err != nil {
			t.Fatalf("scan: %v", err)
		}
		got = append(got, r)
	}
	if len(got) < 20 {
		t.Fatalf("only %d live rows were written; the load did not run", len(got))
	}
	seen := map[string]bool{}
	last := ""
	for i, r := range got {
		if r.part != last {
			if seen[r.part] {
				t.Fatalf("seq %d is in %s, which chain order had already left: partition %s is not a contiguous prefix", r.seq, r.part, r.part)
			}
			seen[r.part] = true
			last = r.part
		}
		if i > 0 && r.at.Before(got[i-1].at) {
			t.Fatalf("recorded_at went backwards at seq %d: %s after %s", r.seq, r.at, got[i-1].at)
		}
	}
	if !seen["ar_p_a"] || !seen["ar_p_b"] {
		t.Errorf("the load did not straddle the boundary (partitions used: %v)", seen)
	}
	if got := chainBreaks(t, f.owner); got != 0 {
		t.Errorf("%d chain break(s) after the concurrent load", got)
	}
	// The high-water mark is the newest row.
	if pgScalar[int64](t, f.owner, `SELECT max(seq) FROM audit_events`) != pgScalar[int64](t, f.owner, `SELECT hw_seq FROM audit_partition_meta`) ||
		pgScalar[string](t, f.owner, `SELECT row_hash FROM audit_events ORDER BY seq DESC LIMIT 1`) != pgScalar[string](t, f.owner, `SELECT hw_row_hash FROM audit_partition_meta`) {
		t.Error("the high-water mark in audit_partition_meta is not the newest row")
	}
}

// secondPool is a second, separate connection pool onto the database and schema of a fixture: what a
// concurrent writer sees, as opposed to the connection that is migrating.
func secondPool(t *testing.T, schema string) *pgxpool.Pool {
	t.Helper()
	u, err := url.Parse(os.Getenv("WARDYN_TEST_PG"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		t.Skip("WARDYN_TEST_PG is not a URL-form DSN")
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	pool, err := Connect(context.Background(), u.String())
	if err != nil {
		t.Fatalf("second connection: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// auditGuardsHold asserts, from the given pool, that nothing unchained can be appended and nothing can
// be mutated or truncated. Every attempt must be refused (or time out on a lock an open migration
// holds, which is also a refusal): none may succeed. Row counts are the caller's to compare, since a
// read can itself wait behind a migration.
func auditGuardsHold(t *testing.T, pool *pgxpool.Pool, when string) {
	t.Helper()
	ctx := context.Background()
	conn, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatalf("%s: acquire: %v", when, err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SET lock_timeout = '1s'`); err != nil {
		t.Fatalf("%s: set lock_timeout: %v", when, err)
	}
	for _, q := range []string{
		`INSERT INTO audit_events (id, actor_type, actor, action, outcome) VALUES (gen_random_uuid(), 'system', 'x', 'test.old', 'success')`,
		`INSERT INTO audit_events (seq, id, actor_type, actor, action, outcome) VALUES (nextval('audit_events_seq'), gen_random_uuid(), 'system', 'x', 'test.explicit', 'success')`,
		`UPDATE audit_events SET actor = 'tampered'`,
		`DELETE FROM audit_events`,
		`TRUNCATE audit_events`,
		`TRUNCATE audit_events_legacy`,
	} {
		if _, err := conn.Exec(ctx, q); err == nil {
			t.Errorf("%s: a second connection ran %q", when, q)
		}
	}
}

// TestPG_AuditPartition_FailureInjectionLeavesNoUnguardedWindow: 0111 commits with working guards, and
// 0112 is one transaction that either completes or leaves 0111's guards in place. The probes run from a
// SECOND connection, because a probe on the migrating connection proves nothing about what a concurrent
// writer sees.
func TestPG_AuditPartition_FailureInjectionLeavesNoUnguardedWindow(t *testing.T) {
	ctx := context.Background()
	owner, schema := partialSchemaPool(t, auditReplayableFile) // 0111 applied and recorded; 0112 not yet
	pgExec(t, owner, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'system', 'seed', 'test.seed', '', 'success', '', NULL)`)
	second := secondPool(t, schema)
	count := func() int { return pgScalar[int](t, owner, `SELECT count(*)::int FROM audit_events`) }
	want := count()

	// A crash after 0111 and before 0112 (the next boot would apply it): the guards are already armed.
	auditGuardsHold(t, second, "after 0111, before 0112")

	// During 0112: its statements ran, nothing committed.
	tx, err := owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.Exec(ctx, readMigration(t, auditReplayableFile)); err != nil {
		tx.Rollback(ctx) //nolint:errcheck
		t.Fatalf("run 0112 inside a transaction: %v", err)
	}
	auditGuardsHold(t, second, "during 0112")

	// 0112 failing at its very end rolls back to 0111's state, still guarded.
	if _, err := tx.Exec(ctx, `SELECT 1/0`); err == nil {
		t.Fatal("the injected failure did not fail")
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	auditGuardsHold(t, second, "after a failed 0112")
	if got := count(); got != want {
		t.Errorf("rows went from %d to %d across the injected failures", want, got)
	}

	// And the real 0112 completes from there.
	if err := Migrate(ctx, owner); err != nil {
		t.Fatalf("Migrate after the injected failures: %v", err)
	}
	auditGuardsHold(t, second, "after 0112")
	if got := count(); got != want {
		t.Errorf("rows went from %d to %d across the guard probes", want, got)
	}
}

// TestPG_AuditPartition_AnOldBinaryAndItsMigratorWriteNoUnchainedRow: a 0.8.5 binary that is let near
// the converted database (the break-glass WARDYN_ALLOW_UNKNOWN_MIGRATIONS, a mistaken rollback) writes
// with a direct INSERT, and its migrator replays the OLD chain trigger files. Neither may produce a row.
func TestPG_AuditPartition_AnOldBinaryAndItsMigratorWriteNoUnchainedRow(t *testing.T) {
	ctx := context.Background()
	f := newUpgradeFixture(t, 3)
	f.convert(t)
	count := func() int { return pgScalar[int](t, f.owner, `SELECT count(*)::int FROM audit_events`) }
	want := count()
	oldInsert := `INSERT INTO audit_events (id, actor_type, actor, action, outcome) VALUES (gen_random_uuid(), 'system', 'old-binary', 'test.old', 'success')`

	if _, err := f.owner.Exec(ctx, oldInsert); err == nil {
		t.Fatal("a 0.8.5 binary's direct INSERT was accepted by the converted table")
	}

	// What 0.8.5's ensureAuditTriggers would run to "restore" the chain trigger: its replay set,
	// verbatim, in one transaction.
	tx, err := f.owner.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	for _, name := range []string{"0047_audit_hash_chain.sql", "0056_audit_chain_serialize.sql",
		"0057_audit_chain_security_definer.sql", "0058_audit_chain_schema_qualified.sql"} {
		if _, err := tx.Exec(ctx, readMigration(t, name)); err != nil {
			tx.Rollback(ctx) //nolint:errcheck
			t.Fatalf("replay %s: %v", name, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit the old replay: %v", err)
	}
	if _, err := f.owner.Exec(ctx, oldInsert); err == nil {
		t.Fatal("after its migrator replayed the pre-0111 trigger files, the 0.8.5 binary's direct INSERT was accepted")
	}
	if got := count(); got != want {
		t.Errorf("rows went from %d to %d: an old binary wrote a row", want, got)
	}

	// And the other half of the refusal: the 0.8.5 binary does not ship 0111/0112, so its own Migrate
	// reads the database as migrated by something newer, which it refuses.
	var shipped []string
	for _, n := range embeddedMigrationNames(t) {
		if n != auditConversionFile && n != auditReplayableFile {
			shipped = append(shipped, n)
		}
	}
	unknown, err := unknownAppliedMigrations(ctx, f.owner, shipped)
	if err != nil {
		t.Fatalf("unknownAppliedMigrations: %v", err)
	}
	if !slices.Contains(unknown, auditConversionFile) || !slices.Contains(unknown, auditReplayableFile) {
		t.Errorf("a binary that does not ship 0111/0112 sees unknown migrations %v, want both", unknown)
	}
}

// TestPG_AuditPartition_ReplayIsIdempotentAndEveryPartitionIsRestored: the boot-time restore replays
// the trigger files over a converted database without changing the parent's index set, and restores a
// chain trigger that was dropped from the parent or disabled on one partition's clone.
func TestPG_AuditPartition_ReplayIsIdempotentAndEveryPartitionIsRestored(t *testing.T) {
	ctx := context.Background()
	pool, _ := probeSchemaPool(t)
	before := auditIndexNames(t, pool, "audit_events")
	for pass := 1; pass <= 2; pass++ {
		if err := replayTriggerMigrations(ctx, pool, auditChainTrigger); err != nil {
			t.Fatalf("replay pass %d: %v", pass, err)
		}
	}
	if after := auditIndexNames(t, pool, "audit_events"); !slices.Equal(before, after) {
		t.Errorf("the parent's indexes changed across a replay: %v -> %v", before, after)
	}

	relations := pgScalar[int](t, pool, `SELECT 1 + count(*)::int FROM pg_inherits WHERE inhparent = 'audit_events'::regclass`)
	chainTriggers := func() (firing int) {
		return pgScalar[int](t, pool, `
			WITH RECURSIVE tree(oid) AS (SELECT 'audit_events'::regclass::oid
			                             UNION ALL SELECT i.inhrelid FROM pg_inherits i JOIN tree ON i.inhparent = tree.oid)
			SELECT count(*)::int FROM pg_trigger t JOIN tree ON tree.oid = t.tgrelid
			 WHERE t.tgname = 'audit_events_chain' AND t.tgenabled IN ('O', 'A')`)
	}
	if got := chainTriggers(); got != relations {
		t.Fatalf("after a replay %d of %d relations fire the chain trigger", got, relations)
	}

	// A clone disabled on one partition: the parent still looks fine.
	leaf := pgScalar[string](t, pool, `SELECT inhrelid::regclass::text FROM pg_inherits WHERE inhparent = 'audit_events'::regclass ORDER BY 1 DESC LIMIT 1`)
	pgExec(t, pool, `ALTER TABLE `+leaf+` DISABLE TRIGGER audit_events_chain`)
	if got := chainTriggers(); got != relations-1 {
		t.Fatalf("disabling the clone on %s left %d firing, want %d", leaf, got, relations-1)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate over a disabled clone: %v", err)
	}
	if got := chainTriggers(); got != relations {
		t.Errorf("after Migrate %d of %d relations fire the chain trigger: a disabled clone was not restored", got, relations)
	}

	// The parent's trigger dropped (and with it every clone).
	pgExec(t, pool, `DROP TRIGGER audit_events_chain ON audit_events`)
	if got := chainTriggers(); got != 0 {
		t.Fatalf("dropping the parent trigger left %d clones firing", got)
	}
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate over a dropped trigger: %v", err)
	}
	if got := chainTriggers(); got != relations {
		t.Errorf("after Migrate %d of %d relations fire the chain trigger: the dropped trigger was not restored", got, relations)
	}
	appendAudit(t, pool, "test.after.restore")
	if got := chainBreaks(t, pool); got != 0 {
		t.Errorf("%d chain break(s) after the restore", got)
	}
}

// TestPG_AuditAppendPostureReportsPublicExecute: a PUBLIC EXECUTE on an audit function is reported.
func TestPG_AuditAppendPostureReportsPublicExecute(t *testing.T) {
	ctx := context.Background()
	pool, _ := probeSchemaPool(t)
	p, err := AuditAppendPostureOf(ctx, pool)
	if err != nil {
		t.Fatalf("AuditAppendPostureOf: %v", err)
	}
	if len(p.PublicExecute) != 0 {
		t.Fatalf("a fresh install reports PUBLIC EXECUTE on %v; 0111 revokes it", p.PublicExecute)
	}
	pgExec(t, pool, `GRANT EXECUTE ON FUNCTION `+auditAppendFn+` TO PUBLIC`)
	p, err = AuditAppendPostureOf(ctx, pool)
	if err != nil {
		t.Fatalf("AuditAppendPostureOf: %v", err)
	}
	if !slices.Equal(p.PublicExecute, []string{"audit_append"}) {
		t.Errorf("PublicExecute = %v, want [audit_append]", p.PublicExecute)
	}
}

// TestPG_AuditPartition_AlwaysHardeningReachesEveryPartition: docs/OPERATIONS.md tells an operator to
// ENABLE ALWAYS the audit triggers so they also fire under session_replication_role = replica. The row
// triggers are cloned, so that command on the parent reaches them; the TRUNCATE guard is a statement
// trigger, one copy per partition, which it does not. Migrate carries the hardening down at every boot,
// and a partition created later by audit_ensure_partitions inherits the parent's state.
func TestPG_AuditPartition_AlwaysHardeningReachesEveryPartition(t *testing.T) {
	ctx := context.Background()
	pool, _ := probeSchemaPool(t)
	names := []string{auditChainTrigger, auditAppendOnlyTriggers[0], auditAppendOnlyTriggers[1]}
	for _, n := range names {
		pgExec(t, pool, `ALTER TABLE audit_events ENABLE ALWAYS TRIGGER `+n)
	}
	notAlways := func() []string {
		rows, err := pool.Query(ctx, `
			WITH RECURSIVE tree(oid) AS (SELECT 'audit_events'::regclass::oid
			                             UNION ALL SELECT i.inhrelid FROM pg_inherits i JOIN tree ON i.inhparent = tree.oid)
			SELECT t.tgrelid::regclass::text || '.' || t.tgname FROM pg_trigger t JOIN tree ON tree.oid = t.tgrelid
			 WHERE t.tgname = ANY($1) AND t.tgenabled <> 'A' ORDER BY 1`, names)
		if err != nil {
			t.Fatalf("read trigger states: %v", err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, s)
		}
		return out
	}
	// Created after the hardening: the new partition's TRUNCATE guard follows the parent's.
	pgExec(t, pool, `SELECT audit_ensure_partitions(24)`)
	newest := pgScalar[string](t, pool, `SELECT m->>'name' FROM audit_partition_meta, jsonb_array_elements(manifest) m ORDER BY m->>'hi' DESC LIMIT 1`)
	if state := pgScalar[string](t, pool, `SELECT tgenabled::text FROM pg_trigger WHERE tgrelid = $1::regclass AND tgname = $2`, newest, auditAppendOnlyTriggers[1]); state != "A" {
		t.Errorf("the TRUNCATE guard on the new partition %s is %q, want 'A' like the parent's", newest, state)
	}
	// The older partitions' TRUNCATE guards are brought along by the next Migrate.
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if got := notAlways(); len(got) != 0 {
		t.Errorf("after Migrate these audit triggers are not ENABLE ALWAYS although the parent's are: %v", got)
	}
	// And through a replay that re-creates every trigger as 'O'.
	unapplyMigrations(t, pool, chainTriggerMigrations(t))
	if err := Migrate(ctx, pool); err != nil {
		t.Fatalf("Migrate after un-applying the trigger files: %v", err)
	}
	if got := notAlways(); len(got) != 0 {
		t.Errorf("after a replay these audit triggers lost ENABLE ALWAYS: %v", got)
	}
}
