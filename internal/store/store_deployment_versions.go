// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// UpgradeOrigin is what this deployment recorded about the release it ran before current (migration
// 0190). Recorded is false when current has no row yet.
type UpgradeOrigin struct {
	Recorded bool
	// Previous is the release booted before current, from its row; "" when current is the first
	// release this deployment recorded.
	Previous string
	// PriorMigration is the newest migration the database held before current first migrated it; ""
	// on a fresh database.
	PriorMigration string
	// LastRunRelease is the newest run's proxy_release other than current: the release that last
	// started a run here. Read only when Previous is "".
	LastRunRelease string
}

// DeploymentVersionStore reads the boot record.
type DeploymentVersionStore interface {
	UpgradeOrigin(ctx context.Context, current string) (UpgradeOrigin, error)
}

var _ DeploymentVersionStore = PG{}

// UpgradeOrigin reads current's row, the row booted before it, and, when there is none, the newest
// run started under another release.
func (s PG) UpgradeOrigin(ctx context.Context, current string) (UpgradeOrigin, error) {
	var o UpgradeOrigin
	var booted time.Time
	err := s.Pool.QueryRow(ctx, `SELECT prior_migration, first_booted_at FROM deployment_versions WHERE version = $1`, current).Scan(&o.PriorMigration, &booted)
	if errors.Is(err, pgx.ErrNoRows) {
		return o, nil
	}
	if err != nil {
		return o, fmt.Errorf("store: read deployment version: %w", err)
	}
	o.Recorded = true
	err = s.Pool.QueryRow(ctx, `SELECT version FROM deployment_versions WHERE first_booted_at < $1 ORDER BY first_booted_at DESC LIMIT 1`, booted).Scan(&o.Previous)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return o, fmt.Errorf("store: read previous deployment version: %w", err)
	}
	if o.Previous != "" {
		return o, nil
	}
	err = s.Pool.QueryRow(ctx, `SELECT proxy_release FROM agent_runs WHERE proxy_release <> '' AND proxy_release <> $1 ORDER BY created_at DESC LIMIT 1`, current).Scan(&o.LastRunRelease)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return o, fmt.Errorf("store: read last run release: %w", err)
	}
	return o, nil
}
