// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live tests for the legacy split (store.SplitLegacyAudit, behind `wardynd -audit-split-legacy`). Guarded by
// WARDYN_TEST_PG like every *_pg_test.go here; the refusal that needs a tampered row needs a superuser.

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// splitRow is one pre-0.8.6 row of the fixture, in seq order.
type splitRow struct {
	time     time.Time
	hashless bool
	action   string
}

// splitFixture is a 0.8.5 chain converted to the partitioned log. The rows are written in this order, and
// the replayed ones carry a "time" OLDER than rows written before them, the way the audit spool's
// at-least-once replay does:
//
//	Nov 2024   2 hashless rows, then 2 chained rows, the second a replay from October
//	Jan 2025   3 rows, the third a replay from December
//	Feb 2025   3 rows, the second a replay from January
//	Apr 2025   2 rows (March has none)
//	now        a row dated 90 days in the FUTURE, then one dated now
//
// The running maximum of "time" in seq order crosses a month at each of those boundaries, so the split must
// make five ranges, and the future-dated row must be clamped below the cutover into the last one.
type splitFixture struct {
	pool *pgxpool.Pool
	rows []splitRow
	seqs []int64
}

func d(y int, m time.Month, day int) time.Time { return time.Date(y, m, day, 12, 0, 0, 0, time.UTC) }

func newSplitFixture(t *testing.T) *splitFixture {
	t.Helper()
	pool := databaseBefore(t, "0111_audit_partitioned.sql")
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	f := &splitFixture{pool: pool, rows: []splitRow{
		{d(2024, 11, 10), true, "split.nov.hashless.1"}, {d(2024, 11, 11), true, "split.nov.hashless.2"},
		{d(2024, 11, 20), false, "split.nov.chained.1"}, {d(2024, 10, 30), false, "split.nov.chained.2.replay"},
		{d(2025, 1, 5), false, "split.jan.1"}, {d(2025, 1, 20), false, "split.jan.2"}, {d(2024, 12, 28), false, "split.jan.3.replay"},
		{d(2025, 2, 3), false, "split.feb.1"}, {d(2025, 1, 15), false, "split.feb.2.replay"}, {d(2025, 2, 10), false, "split.feb.3"},
		{d(2025, 4, 1), false, "split.apr.1"}, {d(2025, 4, 2), false, "split.apr.2"},
		{now.Add(90 * 24 * time.Hour), false, "split.future"}, {now, false, "split.now"},
	}}
	toggle := func(sql string) {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
	for _, r := range f.rows {
		if r.hashless {
			toggle(`ALTER TABLE audit_events DISABLE TRIGGER audit_events_chain`)
		} else {
			toggle(`ALTER TABLE audit_events ENABLE TRIGGER audit_events_chain`)
		}
		var seq int64
		if err := pool.QueryRow(ctx, `INSERT INTO audit_events (id, "time", actor_type, actor, action, outcome, data)
			VALUES (gen_random_uuid(), $1, 'system', 'split-probe', $2, 'success', '{"fixture": true}') RETURNING seq`, r.time, r.action).Scan(&seq); err != nil {
			t.Fatalf("seed %s: %v", r.action, err)
		}
		f.seqs = append(f.seqs, seq)
	}
	toggle(`ALTER TABLE audit_events ENABLE TRIGGER audit_events_chain`)
	for _, name := range []string{"0111_audit_partitioned.sql", "0112_audit_chain_partitioned.sql", "0119_audit_partition_digest.sql", "0123_audit_retention.sql", "0144_audit_legacy_manifest.sql"} {
		execMigrationFile(t, pool, name)
	}
	for i := 0; i < 3; i++ {
		ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "split-probe", Action: fmt.Sprintf("split.post.%d", i), Outcome: "success"}
		if err := store.InsertAuditEvent(ctx, pool, &ev); err != nil {
			t.Fatalf("InsertAuditEvent: %v", err)
		}
	}
	if st := sweep(t, pool); !st.OK || st.Legacy != 2 {
		t.Fatalf("fixture: the chain does not verify as built: %+v", st)
	}
	return f
}

// expected is the grouping the split must produce, worked out here with no database: walk the rows in seq
// order, keep the running maximum of "time" (clamped below the cutover), and start a range when it crosses
// into a new UTC month. The value is each range's seqs in order, keyed by its UTC month.
func (f *splitFixture) expected(cutover time.Time) (months []string, seqs map[string][]int64, maxRec map[string]time.Time) {
	seqs, maxRec = map[string][]int64{}, map[string]time.Time{}
	run := time.Time{}
	for i, r := range f.rows {
		if r.time.After(run) {
			run = r.time
		}
		rec := run
		if last := cutover.Add(-time.Microsecond); rec.After(last) {
			rec = last
		}
		m := rec.UTC().Format("200601")
		if _, ok := seqs[m]; !ok {
			months = append(months, m)
		}
		seqs[m] = append(seqs[m], f.seqs[i])
		maxRec[m] = rec
	}
	return months, seqs, maxRec
}

// split runs the split the way the tool does: one transaction on one connection, committed only on success.
func split(ctx context.Context, pool *pgxpool.Pool) (store.AuditSplitResult, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return store.AuditSplitResult{}, err
	}
	defer conn.Release()
	tx, err := conn.Begin(ctx)
	if err != nil {
		return store.AuditSplitResult{}, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a committed transaction has nothing to undo
	res, err := store.SplitLegacyAudit(ctx, tx)
	if err != nil {
		return res, err
	}
	return res, tx.Commit(ctx)
}

// chainRows reads every row but recorded_at, in seq order, as text: what must not change.
func chainRows(t *testing.T, pool *pgxpool.Pool) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT seq || '|' || id || '|' || extract(epoch FROM "time") || '|' || COALESCE(run_id::text, '') ||
		'|' || actor_type || '|' || actor || '|' || action || '|' || target || '|' || outcome || '|' || source_ip || '|' || COALESCE(data::text, '') ||
		'|' || COALESCE(prev_hash, '') || '|' || COALESCE(row_hash, '') FROM audit_events ORDER BY seq`)
	if err != nil {
		t.Fatalf("read the chain: %v", err)
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

func partitionSeqs(t *testing.T, pool *pgxpool.Pool, name string) []int64 {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT seq FROM `+pgx.Identifier{name}.Sanitize()+` ORDER BY seq`)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var s int64
		if err := rows.Scan(&s); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out = append(out, s)
	}
	return out
}

// TestPG_SplitLegacyAudit_RangesAreSeqContiguousAndEveryRowSurvives: on a seeded 0.8.5 chain with
// spool-replayed rows, the split yields exactly the ranges the running maximum of "time" dictates, each
// holding an unbroken run of seq, each digest equal to the fold over its source rows, no hash changed, and
// verify still inspecting every legacy row.
func TestPG_SplitLegacyAudit_RangesAreSeqContiguousAndEveryRowSurvives(t *testing.T) {
	f := newSplitFixture(t)
	ctx := context.Background()
	cutover := scalar[time.Time](t, f.pool, `SELECT cutover FROM audit_partition_meta`)
	wantMonths, wantSeqs, wantMax := f.expected(cutover)
	if len(wantMonths) != 5 {
		t.Fatalf("fixture expects 5 ranges, the model gives %v", wantMonths)
	}
	before := chainRows(t, f.pool)
	stBefore := sweep(t, f.pool)
	totalBefore := scalar[int64](t, f.pool, `SELECT count(*) FROM audit_events`)

	res, err := split(ctx, f.pool)
	if err != nil {
		t.Fatalf("SplitLegacyAudit: %v", err)
	}

	if len(res.Ranges) != len(wantMonths) {
		t.Fatalf("%d ranges, want %d (%v)", len(res.Ranges), len(wantMonths), wantMonths)
	}
	var sum int64
	for i, r := range res.Ranges {
		m := wantMonths[i]
		if want := "audit_events_legacy_" + m; r.Name != want {
			t.Errorf("range %d is %s, want %s", i, r.Name, want)
		}
		got := partitionSeqs(t, f.pool, r.Name)
		if fmt.Sprint(got) != fmt.Sprint(wantSeqs[m]) {
			t.Errorf("%s holds seqs %v, want %v", r.Name, got, wantSeqs[m])
		}
		// An unbroken run of seq in the WHOLE log: no row of any other range sits between its ends.
		if n := scalar[int64](t, f.pool, `SELECT count(*) FROM audit_events WHERE seq BETWEEN $1 AND $2`, r.SeqLo, r.SeqHi); n != int64(len(got)) {
			t.Errorf("%s spans seq %d..%d but %d rows of the log sit in that span, it holds %d", r.Name, r.SeqLo, r.SeqHi, n, len(got))
		}
		// Eligibility is by the largest "time" the range holds: its upper bound is that, plus a microsecond.
		if !r.RecordedHi.Equal(wantMax[m]) {
			t.Errorf("%s: largest recorded_at %s, want the largest time it holds, %s", r.Name, r.RecordedHi, wantMax[m])
		}
		if hi := scalar[time.Time](t, f.pool, `SELECT (regexp_match(pg_get_expr(c.relpartbound, c.oid), 'TO \(''([^'']+)''\)'))[1]::timestamptz
			FROM pg_class c WHERE c.relname = $1`, r.Name); !hi.Equal(r.RecordedHi.Add(time.Microsecond)) {
			t.Errorf("%s: partition bound %s, want %s", r.Name, hi, r.RecordedHi.Add(time.Microsecond))
		}
		if i > 0 && !r.RecordedLo.After(res.Ranges[i-1].RecordedHi) {
			t.Errorf("%s starts at %s, not after the previous range's %s", r.Name, r.RecordedLo, res.Ranges[i-1].RecordedHi)
		}
		if want := scalar[string](t, f.pool, `SELECT audit_partition_digest($1)`, r.Name); r.Digest != want {
			t.Errorf("%s: digest %s, audit_partition_digest %s", r.Name, r.Digest, want)
		}
		if got := scalar[string](t, f.pool, `SELECT digest FROM audit_chain_anchors WHERE kind = 'split' AND partition_name = $1`, r.Name); got != r.Digest {
			t.Errorf("%s: split anchor digest %s, want %s", r.Name, got, r.Digest)
		}
		if !scalar[bool](t, f.pool, `SELECT EXISTS (SELECT 1 FROM audit_partition_meta m, jsonb_array_elements(m.manifest) e WHERE e->>'name' = $1)`, r.Name) {
			t.Errorf("%s is not in the expected partition manifest", r.Name)
		}
		sum += r.Rows
	}
	if sum != int64(len(f.rows)) || res.Rows != sum {
		t.Errorf("ranges hold %d rows (result says %d), the legacy partition held %d", sum, res.Rows, len(f.rows))
	}
	if last := res.Ranges[len(res.Ranges)-1]; last.RecordedHi.Add(time.Microsecond).After(cutover) {
		t.Errorf("the last range reaches %s, past the cutover %s", last.RecordedHi, cutover)
	}
	if scalar[bool](t, f.pool, `SELECT to_regclass('audit_events_legacy') IS NOT NULL`) {
		t.Error("audit_events_legacy still exists")
	}
	// recorded_at never goes backwards in chain order, across the ranges and into the live partition.
	if n := scalar[int64](t, f.pool, `SELECT count(*) FROM (SELECT recorded_at < lag(recorded_at) OVER (ORDER BY seq) AS back FROM audit_events) x WHERE back`); n != 0 {
		t.Errorf("%d row(s) have a recorded_at earlier than the row before them", n)
	}

	// Nothing but recorded_at changed, and the verifier walks every row it walked before.
	after := chainRows(t, f.pool)
	if len(after) != len(before) {
		t.Fatalf("%d rows after the split, %d before", len(after), len(before))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d changed:\n  before %s\n  after  %s", i, before[i], after[i])
		}
	}
	if total := scalar[int64](t, f.pool, `SELECT count(*) FROM audit_events`); total != totalBefore {
		t.Errorf("audit_events holds %d rows, held %d", total, totalBefore)
	}
	st := sweep(t, f.pool)
	if !st.OK || st.Checked != stBefore.Checked || st.Legacy != stBefore.Legacy || st.Legacy != 2 {
		t.Errorf("verify after the split = %+v, before = %+v", st, stBefore)
	}
	if st.Checked+st.Legacy != totalBefore {
		t.Errorf("verify inspected %d rows, the log holds %d", st.Checked+st.Legacy, totalBefore)
	}

	// The new partitions are guarded like any other: no update, no delete, no truncate, no stray insert.
	for _, sql := range []string{
		`UPDATE audit_events SET actor = 'x' WHERE seq = ` + fmt.Sprint(f.seqs[0]),
		`DELETE FROM audit_events WHERE seq = ` + fmt.Sprint(f.seqs[0]),
		`TRUNCATE audit_events_legacy_` + wantMonths[0],
		`INSERT INTO audit_events_legacy_` + wantMonths[0] + ` (id, actor_type, actor, action, outcome) VALUES (gen_random_uuid(), 'system', 'x', 'x', 'success')`,
	} {
		if _, err := f.pool.Exec(ctx, sql); err == nil {
			t.Errorf("%s was allowed on a split range", sql)
		}
	}

	// A second run finds nothing to split.
	_, err = split(ctx, f.pool)
	wantSplitRefused(t, err, store.SplitNoLegacy)
}

// TestPG_SplitLegacyAudit_TheOldestRangeDropsThroughRetentionAndVerifyPassesFromItsAnchor: the point of the
// split. The oldest range is dropped by the same attested function a live partition is, ranges cannot be
// dropped out of order, and the chain verifies from the drop's anchor.
func TestPG_SplitLegacyAudit_TheOldestRangeDropsThroughRetentionAndVerifyPassesFromItsAnchor(t *testing.T) {
	f := newSplitFixture(t)
	ctx := context.Background()
	res, err := split(ctx, f.pool)
	if err != nil {
		t.Fatalf("SplitLegacyAudit: %v", err)
	}
	if _, err := f.pool.Exec(ctx, `UPDATE audit_partition_meta SET retention_days = 30, pending_days = NULL, pending_effective_at = NULL`); err != nil {
		t.Fatalf("set the window: %v", err)
	}
	pg := store.NewPG(f.pool)
	oldest, second := res.Ranges[0], res.Ranges[1]

	_, err = pg.DropAuditPartition(ctx, second.Name, second.Digest, "operator@example.com")
	var refused *store.AuditRetentionRefused
	if !errors.As(err, &refused) || refused.Reason != store.RetentionNotOldest {
		t.Fatalf("dropping the second range first: %v, want a %s refusal", err, store.RetentionNotOldest)
	}
	dropped, err := pg.DropAuditPartition(ctx, oldest.Name, oldest.Digest, "operator@example.com")
	if err != nil {
		t.Fatalf("drop the oldest range: %v", err)
	}
	if dropped.Rows != oldest.Rows || dropped.Digest != oldest.Digest {
		t.Errorf("drop = %+v, want %d rows with digest %s", dropped, oldest.Rows, oldest.Digest)
	}
	st := sweep(t, f.pool)
	if !st.OK || st.AnchorSeq != oldest.SeqHi {
		t.Fatalf("verify after the drop = %+v, want OK from the anchor at seq %d", st, oldest.SeqHi)
	}
	// The oldest range held the 2 hashless rows: they left with it, and the chain still starts clean.
	if left := scalar[int64](t, f.pool, `SELECT count(*) FROM audit_events`); st.Legacy != 0 || st.Checked != left {
		t.Errorf("verify after the drop checked %d rows (legacy %d), the log holds %d", st.Checked, st.Legacy, left)
	}
	// The next range is now the oldest, and drops too.
	if _, err := pg.DropAuditPartition(ctx, second.Name, second.Digest, "operator@example.com"); err != nil {
		t.Fatalf("drop the second range: %v", err)
	}
	if st := sweep(t, f.pool); !st.OK || st.AnchorSeq != second.SeqHi {
		t.Fatalf("verify after the second drop = %+v, want OK from the anchor at seq %d", st, second.SeqHi)
	}
}

// TestPG_SplitLegacyAudit_RefusesWithoutChangingAnything: every refusal names its reason and leaves the log
// exactly as it was.
func TestPG_SplitLegacyAudit_RefusesWithoutChangingAnything(t *testing.T) {
	ctx := context.Background()

	t.Run("not the migrator", func(t *testing.T) {
		f := newSplitFixture(t)
		role := "wardyn_split_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12]
		if _, err := f.pool.Exec(ctx, `CREATE ROLE `+role+` NOLOGIN`); err != nil {
			t.Fatalf("create role: %v", err)
		}
		t.Cleanup(func() { _, _ = f.pool.Exec(context.Background(), `DROP ROLE IF EXISTS `+role) })
		conn, err := f.pool.Acquire(ctx)
		if err != nil {
			t.Fatalf("acquire: %v", err)
		}
		defer conn.Release()
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		defer tx.Rollback(ctx) //nolint:errcheck // nothing was committed
		if _, err := tx.Exec(ctx, `SET LOCAL ROLE `+role); err != nil {
			t.Fatalf("set role: %v", err)
		}
		_, err = store.SplitLegacyAudit(ctx, tx)
		wantSplitRefused(t, err, store.SplitNotOwner)
	})

	t.Run("not partitioned", func(t *testing.T) {
		pool := databaseBefore(t, "0111_audit_partitioned.sql")
		_, err := split(ctx, pool)
		wantSplitRefused(t, err, store.SplitNotPartitioned)
	})

	t.Run("a range name is taken", func(t *testing.T) {
		f := newSplitFixture(t)
		if _, err := f.pool.Exec(ctx, `CREATE TABLE audit_events_legacy_202501 (x int)`); err != nil {
			t.Fatalf("create the squatter: %v", err)
		}
		before := chainRows(t, f.pool)
		_, err := split(ctx, f.pool)
		wantSplitRefused(t, err, store.SplitNameTaken)
		f.unchanged(t, before)
	})

	t.Run("the chain does not verify", func(t *testing.T) {
		f := newSplitFixture(t)
		requireTriggerBypass(t, f.pool)
		triggersOff(t, f.pool, func(tx pgx.Tx) error {
			_, err := tx.Exec(ctx, `UPDATE audit_events_legacy SET data = '{"edited": true}' WHERE seq = $1`, f.seqs[4])
			return err
		})
		before := chainRows(t, f.pool)
		_, err := split(ctx, f.pool)
		wantSplitRefused(t, err, store.SplitChainBroken)
		f.unchanged(t, before)
	})
}

// unchanged proves a refused split left the legacy partition, every row, and no range or anchor behind.
func (f *splitFixture) unchanged(t *testing.T, before []string) {
	t.Helper()
	if !scalar[bool](t, f.pool, `SELECT to_regclass('audit_events_legacy') IS NOT NULL`) {
		t.Error("the refused split dropped audit_events_legacy")
	}
	if n := scalar[int64](t, f.pool, `SELECT count(*) FROM audit_chain_anchors WHERE kind = 'split'`); n != 0 {
		t.Errorf("the refused split left %d split anchor(s)", n)
	}
	if n := scalar[int64](t, f.pool, `SELECT count(*) FROM pg_class WHERE relname LIKE 'audit_events_legacy_2%' AND relkind = 'r' AND relispartition`); n != 0 {
		t.Errorf("the refused split left %d attached range(s)", n)
	}
	after := chainRows(t, f.pool)
	if len(after) != len(before) {
		t.Fatalf("%d rows after the refused split, %d before", len(after), len(before))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Errorf("row %d changed by a refused split", i)
		}
	}
}

func wantSplitRefused(t *testing.T, err error, reason string) {
	t.Helper()
	var refused *store.AuditSplitRefused
	if !errors.As(err, &refused) || refused.Reason != reason {
		t.Fatalf("split error = %v, want a refusal with reason %s", err, reason)
	}
	if refused.Detail == "" {
		t.Error("the refusal names no detail")
	}
}
