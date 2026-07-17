// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Sandbox ref -> substrate routing rows (migration 0021). This is the Postgres
// implementation of the orchestrator's RefStore seam: the orchestrator
// write-throughs each created sandbox's ref and owning-substrate NAME here so a
// control-plane restart can rehydrate lifecycle routing (Exec/Wait/Attach/
// Status/Stop/Kill — i.e. the kill switch) in multi-substrate deployments.
// Kept out of store.go on purpose (it sits at a lint size boundary).
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// PutRef upserts the ref -> substrate-name row (ref is the primary key).
func (s PG) PutRef(ctx context.Context, ref, substrateName string) error {
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO sandbox_substrates (ref, substrate_name) VALUES ($1, $2)
		ON CONFLICT (ref) DO UPDATE SET substrate_name = EXCLUDED.substrate_name`,
		ref, substrateName,
	)
	if err != nil {
		return fmt.Errorf("store: put sandbox ref: %w", err)
	}
	return nil
}

// GetRef returns the substrate name for ref. A missing row is (found=false,
// nil error) — pre-migration and unknown refs are expected, not errors.
func (s PG) GetRef(ctx context.Context, ref string) (string, bool, error) {
	var name string
	err := s.Pool.QueryRow(ctx,
		`SELECT substrate_name FROM sandbox_substrates WHERE ref = $1`, ref,
	).Scan(&name)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: get sandbox ref: %w", err)
	}
	return name, true, nil
}

// DeleteRef removes the row for ref. Idempotent: deleting a missing ref is nil.
func (s PG) DeleteRef(ctx context.Context, ref string) error {
	if _, err := s.Pool.Exec(ctx, `DELETE FROM sandbox_substrates WHERE ref = $1`, ref); err != nil {
		return fmt.Errorf("store: delete sandbox ref: %w", err)
	}
	return nil
}
