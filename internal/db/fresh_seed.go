// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"fmt"
	"log/slog"
)

// The fresh-install seed of the Wardyn boundary (#3238). A new install starts in hard_limits with
// empty lists and the lowest confinement floor (CC1), which reaches exactly what no_limits reaches:
// the mode is set so an administrator only adds limits, and nothing a run can do changes. An upgrade
// never seeds: its boundary stays no_limits until a person writes it.

// freshSchema reports whether this migration run is the schema's first: the migrator's own tracking
// table does not exist yet. It is read before migrateOn creates that table, resolved through the
// session's search_path as that CREATE is, so a schema outside public is judged as the migrator
// sees it.
func freshSchema(ctx context.Context, db migrationExecutor) (bool, error) {
	var fresh bool
	if err := db.QueryRow(ctx, `SELECT to_regclass('schema_migrations') IS NULL`).Scan(&fresh); err != nil {
		return false, fmt.Errorf("db: look up schema_migrations: %w", err)
	}
	return fresh, nil
}

// seedFreshBoundary writes the seed once, on the schema's first migration run, before the server
// listens: one transaction that moves a boundary row never seeded and never written
// (seeded_at IS NULL; every write sets it) into hard_limits, with one governance.boundary.seed audit
// row (actor system), appended under the chain lock as every audit writer does. A row a write
// reached first is left alone, so the person's document wins. On a schema that is not fresh it
// only logs, at info, that the boundary stays as it is, since a first boot that stopped before its
// seed restarts not-fresh in no_limits, which is the upgrade posture.
func seedFreshBoundary(ctx context.Context, db migrationExecutor, fresh bool) error {
	if !fresh {
		var unseeded bool
		if err := db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM governance_boundary WHERE singleton AND seeded_at IS NULL)`).Scan(&unseeded); err != nil {
			return fmt.Errorf("db: read the Wardyn boundary seed: %w", err)
		}
		if unseeded {
			slog.InfoContext(ctx, "db: the Wardyn boundary was never seeded or written, so it stays in no_limits "+
				"(an upgraded install, or a first boot that stopped before its seed); set it with PUT /governance/boundary")
		}
		return nil
	}
	tx, err := beginReadCommitted(ctx, db)
	if err != nil {
		return fmt.Errorf("db: begin the Wardyn boundary seed: %w", err)
	}
	defer tx.Rollback(context.WithoutCancel(ctx)) //nolint:errcheck // a committed tx makes this a no-op
	tag, err := tx.Exec(ctx, `UPDATE governance_boundary
		SET mode = 'hard_limits', spec = '{"min_confinement_class":"CC1"}', seeded_at = now(), updated_by = 'system', updated_at = now()
		WHERE singleton AND seeded_at IS NULL`)
	if err != nil {
		return fmt.Errorf("db: seed the Wardyn boundary: %w", err)
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, AuditChainLockTimeoutSQL()); err != nil {
			return fmt.Errorf("db: bound the seed's chain lock wait: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, AuditChainLockKey); err != nil {
			return fmt.Errorf("db: take the chain lock for the seed: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT audit_append(gen_random_uuid(), now(), NULL, 'system', 'system',
			'governance.boundary.seed', 'boundary', 'success', '', '{"mode":"hard_limits","min_confinement_class":"CC1"}'::jsonb)`); err != nil {
			return fmt.Errorf("db: audit the Wardyn boundary seed: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("db: commit the Wardyn boundary seed: %w", err)
	}
	if tag.RowsAffected() == 1 {
		slog.InfoContext(ctx, "db: fresh install: the Wardyn boundary starts in hard_limits with no limits listed and floor CC1")
	}
	return nil
}
