// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
)

// seededBoundary is the boundary row and the count of governance.boundary.seed audit rows.
type seededBoundary struct {
	mode, spec, updatedBy string
	seeded                bool
	seeds                 int
}

func readSeed(t *testing.T, pool *pgxpool.Pool) seededBoundary {
	t.Helper()
	var b seededBoundary
	if err := pool.QueryRow(t.Context(), `SELECT mode, spec::text, updated_by, seeded_at IS NOT NULL FROM governance_boundary WHERE singleton`).
		Scan(&b.mode, &b.spec, &b.updatedBy, &b.seeded); err != nil {
		t.Fatalf("read the boundary: %v", err)
	}
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action = 'governance.boundary.seed'`).Scan(&b.seeds); err != nil {
		t.Fatalf("count the seed audit rows: %v", err)
	}
	return b
}

func migrateOrFail(t *testing.T, pool *pgxpool.Pool) {
	t.Helper()
	if err := Migrate(context.Background(), pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
}

// TestFreshSeedHardLimitsOnce: a fresh install's first boot seeds hard_limits with floor CC1 and
// one audit row; a second boot writes nothing.
func TestFreshSeedHardLimitsOnce(t *testing.T) {
	pool := pgDatabaseIsolated(t)
	migrateOrFail(t, pool)
	b := readSeed(t, pool)
	if b.mode != "hard_limits" || b.spec != `{"min_confinement_class": "CC1"}` || !b.seeded || b.updatedBy != "system" || b.seeds != 1 {
		t.Fatalf("after the first boot: %+v, want hard_limits, floor CC1, seeded by system, one audit row", b)
	}
	migrateOrFail(t, pool)
	if again := readSeed(t, pool); again != b {
		t.Errorf("a second boot changed the seed: %+v, want %+v", again, b)
	}
}

// TestFreshSeedKeepsAnAdminEdit: a boundary a person wrote after the seed is what a restart keeps.
func TestFreshSeedKeepsAnAdminEdit(t *testing.T) {
	pool := pgDatabaseIsolated(t)
	migrateOrFail(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE governance_boundary SET mode = 'no_limits', spec = '{}', updated_by = 'admin',
		seeded_at = COALESCE(seeded_at, now()) WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	migrateOrFail(t, pool)
	if b := readSeed(t, pool); b.mode != "no_limits" || b.updatedBy != "admin" || b.seeds != 1 {
		t.Errorf("after a restart: %+v, want the admin's no_limits kept and still one seed row", b)
	}
}

// TestFreshSeedRacingReplicasWriteOnce: two replicas booting one fresh database seed it once.
func TestFreshSeedRacingReplicasWriteOnce(t *testing.T) {
	pool := pgDatabaseIsolated(t)
	other, err := Connect(context.Background(), pool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(other.Close)
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, p := range []*pgxpool.Pool{pool, other} {
		wg.Go(func() { errs[i] = migrateTolerant(context.Background(), p) })
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	if b := readSeed(t, pool); b.mode != "hard_limits" || b.seeds != 1 {
		t.Errorf("after two racing boots: %+v, want hard_limits and one seed row", b)
	}
}

// TestFreshSeedLosesToAWriteThatLandsFirst: a boundary a person wrote before the seed ran (another
// replica already serving) is left alone, and no seed row is written.
func TestFreshSeedLosesToAWriteThatLandsFirst(t *testing.T) {
	pool := pgDatabaseIsolated(t)
	migrateOrFail(t, pool)
	before := readSeed(t, pool)
	if _, err := pool.Exec(t.Context(), `UPDATE governance_boundary SET mode = 'no_limits', spec = '{}', seeded_at = NULL WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	// The store's write (store.PutBoundaryQ) sets seeded_at on every write.
	if _, err := pool.Exec(t.Context(), `UPDATE governance_boundary SET spec = '{"allowed_domains":["pypi.org"]}', updated_by = 'admin',
		seeded_at = COALESCE(seeded_at, now()) WHERE singleton`); err != nil {
		t.Fatal(err)
	}
	if err := seedFreshBoundary(context.Background(), pool, true); err != nil {
		t.Fatal(err)
	}
	if b := readSeed(t, pool); b.mode != "no_limits" || b.updatedBy != "admin" || b.seeds != before.seeds {
		t.Errorf("after a seed that came second: %+v, want the admin's write and no new seed row", b)
	}
}

// TestFreshSeedNeverSeedsAnUpgrade: an install migrated before the boundary existed upgrades to a
// boundary in no_limits, never seeded, with no seed row.
func TestFreshSeedNeverSeedsAnUpgrade(t *testing.T) {
	pool := pgDatabaseIsolated(t)
	migrateOrFail(t, pool)
	// Back to the schema before 0200: the table gone and both files un-applied.
	if _, err := pool.Exec(t.Context(), `DROP TABLE governance_boundary;
		ALTER TABLE agent_runs DROP COLUMN break_glass;
		DELETE FROM schema_migrations WHERE filename IN ('0200_governance_boundary.sql', '0201_governance_boundary_seed.sql')`); err != nil {
		t.Fatal(err)
	}
	seedsBefore := readSeedCount(t, pool)
	migrateOrFail(t, pool)
	var mode string
	var seeded bool
	if err := pool.QueryRow(t.Context(), `SELECT mode, seeded_at IS NOT NULL FROM governance_boundary WHERE singleton`).Scan(&mode, &seeded); err != nil {
		t.Fatal(err)
	}
	if mode != "no_limits" || seeded || readSeedCount(t, pool) != seedsBefore {
		t.Errorf("after an upgrade: mode %s seeded %v, want no_limits, never seeded, no new seed row", mode, seeded)
	}
}

func readSeedCount(t *testing.T, pool *pgxpool.Pool) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(t.Context(), `SELECT count(*) FROM audit_events WHERE action = 'governance.boundary.seed'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
