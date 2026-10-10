// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

// Live tests for the legacy partition's entry in the expected-partition manifest (migration 0140, #1809).
// Guarded by WARDYN_TEST_PG like every *_pg_test.go here.

import (
	"context"
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

var auditConversion = []string{"0111_audit_partitioned.sql", "0112_audit_chain_partitioned.sql", "0119_audit_partition_digest.sql", "0123_audit_retention.sql"}

const legacyManifestMigration = "0140_audit_legacy_manifest.sql"

// convertedHashlessDatabase is a 0.8.5 database whose audit log holds only hashless rows (the shape of a
// log from before 0047), converted by 0111 with nothing appended since. futureRow adds one dated beyond
// the conversion instant, as an application clock ahead of the database's would stamp it.
func convertedHashlessDatabase(t *testing.T, withManifestEntry, futureRow bool) *pgxpool.Pool {
	t.Helper()
	pool := databaseBefore(t, "0111_audit_partitioned.sql")
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events DISABLE TRIGGER audit_events_chain`); err != nil {
		t.Fatalf("disable the chain trigger: %v", err)
	}
	times := []time.Time{d(2024, 11, 10), d(2024, 11, 11), d(2024, 11, 12)}
	if futureRow {
		times = append(times, time.Now().UTC().Add(90*24*time.Hour))
	}
	for i, at := range times {
		if _, err := pool.Exec(ctx, `INSERT INTO audit_events (id, "time", actor_type, actor, action, outcome, data)
			VALUES (gen_random_uuid(), $1, 'system', 'legacy-probe', $2, 'success', '{}')`,
			at, fmt.Sprintf("legacy.hashless.%d", i)); err != nil {
			t.Fatalf("seed a hashless row: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `ALTER TABLE audit_events ENABLE TRIGGER audit_events_chain`); err != nil {
		t.Fatalf("enable the chain trigger: %v", err)
	}
	for _, name := range auditConversion {
		execMigrationFile(t, pool, name)
	}
	if withManifestEntry {
		execMigrationFile(t, pool, legacyManifestMigration)
	}
	return pool
}

// hashlessLegacyDatabase is convertedHashlessDatabase given one chained row, so the high-water mark sits
// on a row that survives the removal of the legacy partition.
func hashlessLegacyDatabase(t *testing.T, withManifestEntry bool) *pgxpool.Pool {
	t.Helper()
	pool := convertedHashlessDatabase(t, withManifestEntry, false)
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC(), ActorType: types.ActorSystem, Actor: "legacy-probe", Action: "legacy.post", Outcome: "success"}
	if err := store.InsertAuditEvent(context.Background(), pool, &ev); err != nil {
		t.Fatalf("InsertAuditEvent: %v", err)
	}
	if st := sweep(t, pool); !st.OK || st.Legacy != 3 {
		t.Fatalf("fixture: the chain does not verify as built: %+v", st)
	}
	return pool
}

func removeLegacyPartition(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	ctx := context.Background()
	for _, sql := range []string{`ALTER TABLE audit_events DETACH PARTITION audit_events_legacy`, `DROP TABLE audit_events_legacy`} {
		if _, err := pool.Exec(ctx, sql); err != nil {
			t.Fatalf("%s: %v", sql, err)
		}
	}
}

func legacyManifestEntries(t *testing.T, pool *pgxpool.Pool) int64 {
	t.Helper()
	return scalar[int64](t, pool, `SELECT count(*) FROM audit_partition_meta, jsonb_array_elements(manifest) e WHERE e->>'name' = 'audit_events_legacy'`)
}

// TestPG_VerifyAuditChain_RemovedHashlessLegacyPartitionIsMissing: an unattested removal of a legacy
// partition that holds only hashless rows leaves no chained row to miss, so only the manifest can name it.
// Without the seeded entry the removal verifies clean.
func TestPG_VerifyAuditChain_RemovedHashlessLegacyPartitionIsMissing(t *testing.T) {
	pool := hashlessLegacyDatabase(t, true)
	removeLegacyPartition(t, pool)
	st := sweep(t, pool)
	if st.OK || !strings.Contains(st.Reason, "audit_events_legacy") || !strings.Contains(st.Reason, "missing") {
		t.Fatalf("verify after removing the legacy partition = %+v, want a missing-partition finding naming audit_events_legacy", st)
	}
}

// TestPG_Migration0140_BackfillsAConvertedDatabaseOnce: a database converted before 0140 has an empty
// manifest; the migration seeds the legacy entry with the cutover as its upper bound, a replay adds nothing,
// and the database still verifies and still creates its next partitions from the cutover.
func TestPG_Migration0140_BackfillsAConvertedDatabaseOnce(t *testing.T) {
	pool := hashlessLegacyDatabase(t, false)
	if n := legacyManifestEntries(t, pool); n != 0 {
		t.Fatalf("fixture: %d legacy manifest entries before 0140", n)
	}
	execMigrationFile(t, pool, legacyManifestMigration)
	execMigrationFile(t, pool, legacyManifestMigration)
	if n := legacyManifestEntries(t, pool); n != 1 {
		t.Fatalf("%d legacy manifest entries after 0140 applied twice, want 1", n)
	}
	if !scalar[bool](t, pool, `SELECT (e->>'hi')::timestamptz = m.cutover AND (e->>'lo')::timestamptz = '-infinity'
		FROM audit_partition_meta m, jsonb_array_elements(m.manifest) e WHERE e->>'name' = 'audit_events_legacy'`) {
		t.Error("the legacy entry does not run from MINVALUE to the cutover")
	}
	if st := sweep(t, pool); !st.OK {
		t.Fatalf("verify after the backfill = %+v", st)
	}
	if _, err := store.NewPG(pool).EnsureAuditPartitions(context.Background(), 14); err != nil {
		t.Fatalf("EnsureAuditPartitions after the backfill: %v", err)
	}
	if st := sweep(t, pool); !st.OK {
		t.Fatalf("verify after creating partitions = %+v", st)
	}
}

// TestPG_Migration0140_ReportsALegacyPartitionRemovedBeforeIt: a database whose legacy partition was removed
// without an anchor before the upgrade (the AR-02 scenario) is backfilled all the same, so verify reports it
// missing instead of staying clean for good.
func TestPG_Migration0140_ReportsALegacyPartitionRemovedBeforeIt(t *testing.T) {
	pool := hashlessLegacyDatabase(t, false)
	removeLegacyPartition(t, pool)
	if st := sweep(t, pool); !st.OK {
		t.Fatalf("fixture: without the entry the removal verifies clean, got %+v", st)
	}
	execMigrationFile(t, pool, legacyManifestMigration)
	st := sweep(t, pool)
	if st.OK || !strings.Contains(st.Reason, "audit_events_legacy") {
		t.Fatalf("verify after the backfill = %+v, want the legacy partition reported missing", st)
	}
}

// TestPG_Migration0140_LeavesASplitDatabaseAlone: once the legacy table is gone (split or dropped), a
// replay of the migration must not resurrect an entry the verifier would then call missing.
func TestPG_Migration0140_LeavesASplitDatabaseAlone(t *testing.T) {
	f := newSplitFixture(t)
	if _, err := split(context.Background(), f.pool); err != nil {
		t.Fatalf("SplitLegacyAudit: %v", err)
	}
	execMigrationFile(t, f.pool, legacyManifestMigration)
	if n := legacyManifestEntries(t, f.pool); n != 0 {
		t.Fatalf("%d legacy manifest entries after a split and a replay of 0140, want 0", n)
	}
	if st := sweep(t, f.pool); !st.OK {
		t.Fatalf("verify = %+v", st)
	}
}

// TestPG_SplitLegacyAudit_ReplacesTheLegacyManifestEntryWithItsRanges: a split on a seeded manifest swaps
// the legacy entry for the ranges and verifies clean.
func TestPG_SplitLegacyAudit_ReplacesTheLegacyManifestEntryWithItsRanges(t *testing.T) {
	f := newSplitFixture(t)
	if n := legacyManifestEntries(t, f.pool); n != 1 {
		t.Fatalf("fixture: %d legacy manifest entries before the split, want 1", n)
	}
	res, err := split(context.Background(), f.pool)
	if err != nil {
		t.Fatalf("SplitLegacyAudit on a seeded manifest: %v", err)
	}
	if n := legacyManifestEntries(t, f.pool); n != 0 {
		t.Errorf("%d legacy manifest entries after the split, want 0", n)
	}
	for _, r := range res.Ranges {
		if n := scalar[int64](t, f.pool, `SELECT count(*) FROM audit_partition_meta, jsonb_array_elements(manifest) e WHERE e->>'name' = $1`, r.Name); n != 1 {
			t.Errorf("range %s has %d manifest entries, want 1", r.Name, n)
		}
	}
	if st := sweep(t, f.pool); !st.OK {
		t.Fatalf("verify after the split = %+v", st)
	}
}

// TestPG_AuditRetentionDrop_LegacyPartitionRemovesItsManifestEntry: an attested drop of the whole legacy
// partition takes its manifest entry with it, so verify does not call the dropped table missing.
func TestPG_AuditRetentionDrop_LegacyPartitionRemovesItsManifestEntry(t *testing.T) {
	pool := hashlessLegacyDatabase(t, true)
	ctx := context.Background()
	if _, err := pool.Exec(ctx, `UPDATE audit_partition_meta SET retention_days = 30, pending_days = NULL, pending_effective_at = NULL`); err != nil {
		t.Fatalf("set the window: %v", err)
	}
	requireTriggerBypass(t, pool)
	// The legacy partition ends at the conversion instant, which is inside any window: re-bound it to 2020
	// (rows included) so the oldest partition is eligible, as a log converted years ago would be.
	triggersOff(t, pool, func(tx pgx.Tx) error {
		for _, sql := range []string{
			`UPDATE audit_events_legacy SET recorded_at = '2019-06-01'`,
			`ALTER TABLE audit_events DETACH PARTITION audit_events_legacy`,
			`ALTER TABLE audit_events_legacy DROP CONSTRAINT legacy_bound`,
			`ALTER TABLE audit_events_legacy ADD CONSTRAINT legacy_bound CHECK (recorded_at < '2020-01-01')`,
			`ALTER TABLE audit_events ATTACH PARTITION audit_events_legacy FOR VALUES FROM (MINVALUE) TO ('2020-01-01')`,
		} {
			if _, err := tx.Exec(ctx, sql); err != nil {
				return fmt.Errorf("%s: %w", sql, err)
			}
		}
		return nil
	})
	digest := scalar[string](t, pool, `SELECT audit_partition_digest('audit_events_legacy')`)
	if _, err := store.NewPG(pool).DropAuditPartition(ctx, "audit_events_legacy", digest, "operator@example.com"); err != nil {
		t.Fatalf("DropAuditPartition of the legacy partition: %v", err)
	}
	if n := legacyManifestEntries(t, pool); n != 0 {
		t.Errorf("%d legacy manifest entries after the attested drop, want 0", n)
	}
	if st := sweep(t, pool); !st.OK {
		t.Fatalf("verify after the attested drop = %+v", st)
	}
}

// TestPG_SplitLegacyAudit_RightAfterTheConversionWithARowAtTheCutover: `-audit-split-legacy` straight after
// `-migrate-only` (nothing appended, so the high-water mark is still the conversion instant) with a legacy
// row dated at or past the cutover. The last range's bound used to be the cutover, above the high-water
// mark, and its digest refused as still open until a boot and a retry.
func TestPG_SplitLegacyAudit_RightAfterTheConversionWithARowAtTheCutover(t *testing.T) {
	pool := convertedHashlessDatabase(t, true, true)
	res, err := split(context.Background(), pool)
	if err != nil {
		t.Fatalf("SplitLegacyAudit right after the conversion: %v", err)
	}
	if len(res.Ranges) == 0 {
		t.Fatal("the split made no ranges")
	}
	if st := sweep(t, pool); !st.OK {
		t.Fatalf("verify after the split = %+v", st)
	}
}
