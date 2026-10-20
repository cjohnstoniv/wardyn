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
