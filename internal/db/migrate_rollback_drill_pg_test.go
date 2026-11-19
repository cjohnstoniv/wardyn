// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"
)

// lastMigrationOfPriorRelease is the newest migration v0.9.9 ships, the release
// before the 0.10 line. Every migration after it is one a v0.9.9 wardynd does
// not know.
const lastMigrationOfPriorRelease = "0191_secrets_value_rev.sql"

// TestPG_RollbackDrill_PriorReleaseRefusesMigratedDatabase pins the rollback
// the upgrade notes describe, against this tree's REAL migrations: the prior
// release's refusal predicate (unknownAppliedMigrations over the names it
// ships, plus retiredMigrations as migrateOn adds them) finds every migration
// after lastMigrationOfPriorRelease on a database this tree migrated, names
// this tree's newest as the newest, and finds nothing on the schema_migrations
// set a pre-upgrade backup carries. scripts/rollback-drill.sh runs the same
// path with two real binaries and a real pg_dump/restore.
func TestPG_RollbackDrill_PriorReleaseRefusesMigratedDatabase(t *testing.T) {
	pool, _ := probeSchemaPool(t) // a complete schema, every migration of this tree recorded
	ctx := context.Background()

	var shippedByPrior, newerThanPrior []string
	for _, name := range readMigrationNames(t) {
		if name <= lastMigrationOfPriorRelease {
			shippedByPrior = append(shippedByPrior, name)
		} else {
			newerThanPrior = append(newerThanPrior, name)
		}
	}
	if len(newerThanPrior) == 0 {
		t.Fatalf("no migration after %s: there is nothing for the prior release to refuse", lastMigrationOfPriorRelease)
	}
	if !slices.Contains(shippedByPrior, lastMigrationOfPriorRelease) {
		t.Fatalf("%s is not shipped: lastMigrationOfPriorRelease names a migration the prior release does not have", lastMigrationOfPriorRelease)
	}
	shipped := append(shippedByPrior, slices.Sorted(maps.Keys(retiredMigrations))...)

	unknown, err := unknownAppliedMigrations(ctx, pool, shipped)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(unknown, newerThanPrior) {
		t.Fatalf("the prior release would see %v as migrated by a newer wardynd, want every migration after %s: %v",
			unknown, lastMigrationOfPriorRelease, newerThanPrior)
	}
	newest, err := NewestMigration()
	if err != nil {
		t.Fatal(err)
	}
	if got := unknown[len(unknown)-1]; got != newest {
		t.Errorf("the newest unknown migration is %q, want this tree's newest %q", got, newest)
	}

	// The backup taken before the upgrade records only what the prior release
	// shipped; restoring it leaves nothing for the prior release to refuse.
	if _, err := pool.Exec(ctx, `DELETE FROM schema_migrations WHERE filename > $1 COLLATE "C"`, lastMigrationOfPriorRelease); err != nil {
		t.Fatalf("model the restored pre-upgrade schema_migrations: %v", err)
	}
	unknown, err = unknownAppliedMigrations(ctx, pool, shipped)
	if err != nil {
		t.Fatal(err)
	}
	if len(unknown) != 0 {
		t.Errorf("the restored pre-upgrade set still reads as migrated by a newer wardynd: %s", strings.Join(unknown, ", "))
	}
}
