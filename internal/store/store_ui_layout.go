// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Per-user run-detail cockpit widget-layout persistence (migration 0037).
// Kept out of store.go/iface.go on purpose — see RunLayoutStore's doc below.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// RunLayoutStore is the run-cockpit widget-layout persistence surface.
// Deliberately NOT part of the Store interface: ~30 test doubles embed
// store.Store, and widening it would break every one for a feature they have
// nothing to do with. The api layer type-asserts s.cfg.Store to
// RunLayoutStore and degrades when absent (GET returns the default shape,
// PUT 501s); production is always PG, which has it.
type RunLayoutStore interface {
	// GetRunLayout returns ErrNotFound when nothing has been saved yet; the
	// api layer treats that as the default shape, not an error.
	GetRunLayout(ctx context.Context, principal, preset string) (types.RunLayout, error)
	// PutRunLayout upserts principal's layout for preset and returns the
	// stored row, including the server-assigned updated_at.
	PutRunLayout(ctx context.Context, principal, preset string, layout []types.RunLayoutWidget) (types.RunLayout, error)
}

// Compile-time assertion: PG satisfies RunLayoutStore.
var _ RunLayoutStore = PG{}

// GetRunLayout reads the one (principal, preset) row scoped to principal — a
// layout is never looked up by anything but its owner.
func (s PG) GetRunLayout(ctx context.Context, principal, preset string) (types.RunLayout, error) {
	const q = `SELECT preset, layout, updated_at FROM ui_run_layouts WHERE principal = $1 AND preset = $2`
	return scanRunLayout(s.Pool.QueryRow(ctx, q, principal, preset))
}

// PutRunLayout upserts principal's layout for preset. ON CONFLICT DO UPDATE
// makes this one idempotent write with no read-then-decide round trip and no
// lost-update race between two tabs saving the same preset back to back.
func (s PG) PutRunLayout(ctx context.Context, principal, preset string, layout []types.RunLayoutWidget) (types.RunLayout, error) {
	if layout == nil {
		// json.Marshal(nil) emits `null`, not `[]` — keep the "empty, never
		// nil" shape this package promises everywhere else.
		layout = []types.RunLayoutWidget{}
	}
	raw, err := json.Marshal(layout)
	if err != nil {
		return types.RunLayout{}, fmt.Errorf("store: marshal run layout: %w", err)
	}
	const q = `
		INSERT INTO ui_run_layouts (principal, preset, layout, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT (principal, preset) DO UPDATE
			SET layout = EXCLUDED.layout, updated_at = EXCLUDED.updated_at
		RETURNING preset, layout, updated_at`
	return scanRunLayout(s.Pool.QueryRow(ctx, q, principal, preset, raw))
}

func scanRunLayout(row pgx.Row) (types.RunLayout, error) {
	var l types.RunLayout
	var layoutRaw []byte
	err := row.Scan(&l.Preset, &layoutRaw, &l.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunLayout{}, ErrNotFound
	}
	if err != nil {
		return types.RunLayout{}, fmt.Errorf("store: scan run layout: %w", err)
	}
	if err := json.Unmarshal(layoutRaw, &l.Layout); err != nil {
		return types.RunLayout{}, fmt.Errorf("store: unmarshal run layout: %w", err)
	}
	if l.Layout == nil {
		// Defensive: should be unreachable given the column's NOT NULL
		// DEFAULT '[]'::jsonb, but never hand the caller a nil slice.
		l.Layout = []types.RunLayoutWidget{}
	}
	return l, nil
}
