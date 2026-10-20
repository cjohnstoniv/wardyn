// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
)

// This file holds the one statement that runs on the migrator connection
// (migrateRecordingVersion); scripts/test-claims-match-code.sh exempts the whole file from the
// application-role verb check, so nothing else may be added here.

// RecordDeploymentVersion records that this deployment booted version (migration 0190), once per
// release. priorMigration is NewestAppliedMigration read before this boot migrated. Two servers of
// one release starting together each read it before the other finished migrating or after, so the
// stored value is the older of the two: the one read before the release first migrated.
func RecordDeploymentVersion(ctx context.Context, db migrationExecutor, version, priorMigration string) error {
	if _, err := db.Exec(ctx, `
		INSERT INTO deployment_versions (version, prior_migration) VALUES ($1, $2)
		ON CONFLICT (version) DO UPDATE
		   SET prior_migration = LEAST(deployment_versions.prior_migration COLLATE "C", EXCLUDED.prior_migration COLLATE "C")`,
		version, priorMigration); err != nil {
		return fmt.Errorf("db: record deployment version: %w", err)
	}
	return nil
}
