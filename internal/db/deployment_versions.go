// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
)

// NewestAppliedMigration is the newest filename schema_migrations records, or "" on a database no
// wardynd has migrated yet. wardynd reads it before migrating, so RecordDeploymentVersion can keep what
// the database held before this release.
func NewestAppliedMigration(ctx context.Context, db migrationExecutor) (string, error) {
	var exists bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&exists); err != nil {
		return "", fmt.Errorf("db: find schema_migrations: %w", err)
	}
	if !exists {
		return "", nil
	}
	var newest string
	if err := db.QueryRow(ctx, `SELECT COALESCE(max(filename COLLATE "C"), '') FROM schema_migrations`).Scan(&newest); err != nil {
		return "", fmt.Errorf("db: read newest migration: %w", err)
	}
	return newest, nil
}

// RecordDeploymentVersion records that this deployment booted version (migration 0192), once per
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
