// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/version"
)

// pgExecutor is what internal/db's migration steps run on: a *pgxpool.Pool or a *pgxpool.Conn.
type pgExecutor interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Begin(ctx context.Context) (pgx.Tx, error)
}

// migrateRecordingVersion runs apply (the migrations, on q) and records this release in
// deployment_versions (migration 0190), keeping the newest migration the database held before, so
// setup status can name the defaults an upgrade changed. Both a boot and -migrate-only call it: a
// stopped-writer upgrade migrates in the Job, and the serving pod's boot then finds nothing pending.
// A record that cannot be written is logged, not fatal: it feeds an information row, and that row is
// left out while this release has no record.
func migrateRecordingVersion(ctx context.Context, q pgExecutor, apply func() error) error {
	prior, priorErr := db.NewestAppliedMigration(ctx, q)
	if err := apply(); err != nil {
		return err
	}
	if priorErr == nil {
		priorErr = db.RecordDeploymentVersion(ctx, q, version.Version, prior)
	}
	if priorErr != nil {
		slog.WarnContext(ctx, "wardynd: could not record this release in deployment_versions; setup status leaves out the defaults this upgrade changed", slog.Any("err", priorErr))
	}
	return nil
}
