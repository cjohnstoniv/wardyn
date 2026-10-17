// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The boot record (migration 0192): the newest migration read before migrating is "" on a fresh
// database; a release's row keeps the oldest prior migration any of its boots read; the release booted
// before is the previous row; and a first record also reads the newest run started under another release.
func TestPG_DeploymentVersions_RecordAndOrigin(t *testing.T) {
	ctx := context.Background()
	pool := throwawayDatabase(t)
	if prior, err := db.NewestAppliedMigration(ctx, pool); err != nil || prior != "" {
		t.Fatalf("NewestAppliedMigration on a fresh database = %q, %v; want empty", prior, err)
	}
	if err := db.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	newest, err := db.NewestMigration()
	if err != nil {
		t.Fatal(err)
	}
	if prior, err := db.NewestAppliedMigration(ctx, pool); err != nil || prior != newest {
		t.Fatalf("NewestAppliedMigration after migrating = %q, %v; want %q", prior, err, newest)
	}
	pg := store.NewPG(pool)

	if o, err := pg.UpgradeOrigin(ctx, "1.0.0-rc.1"); err != nil || o.Recorded {
		t.Fatalf("origin before any record = %+v, %v; want not recorded", o, err)
	}
	// Two boots of one release: the second read its prior migration after the first had migrated.
	if err := db.RecordDeploymentVersion(ctx, pool, "1.0.0-rc.1", "0179_runner_arch.sql"); err != nil {
		t.Fatal(err)
	}
	if err := db.RecordDeploymentVersion(ctx, pool, "1.0.0-rc.1", newest); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i, release := range []string{"0.9.2", "1.0.0-rc.1"} { // the newest run is this release's own, which is skipped
		run, err := pg.CreateRun(ctx, types.AgentRun{
			ID: uuid.New(), CreatedAt: now.Add(time.Duration(i) * time.Minute), UpdatedAt: now,
			CreatedBy: "op@example.com", Agent: "claude-code", Task: "release record",
			ConfinementClass: types.CC1, State: types.RunPending,
			SPIFFEID: "spiffe://test/agent-run/" + uuid.NewString(), RunnerTarget: "docker",
		})
		if err != nil {
			t.Fatalf("create run: %v", err)
		}
		if _, err := pool.Exec(ctx, `UPDATE agent_runs SET proxy_release = $1 WHERE id = $2`, release, run.ID); err != nil {
			t.Fatal(err)
		}
	}
	o, err := pg.UpgradeOrigin(ctx, "1.0.0-rc.1")
	if err != nil || o != (store.UpgradeOrigin{Recorded: true, PriorMigration: "0179_runner_arch.sql", LastRunRelease: "0.9.2"}) {
		t.Fatalf("first record's origin = %+v, %v; want the older prior migration and the last other run release", o, err)
	}
	if err := db.RecordDeploymentVersion(ctx, pool, "1.0.0", newest); err != nil {
		t.Fatal(err)
	}
	o, err = pg.UpgradeOrigin(ctx, "1.0.0")
	if err != nil || o != (store.UpgradeOrigin{Recorded: true, Previous: "1.0.0-rc.1", PriorMigration: newest}) {
		t.Fatalf("second release's origin = %+v, %v; want the release booted before it", o, err)
	}
}
