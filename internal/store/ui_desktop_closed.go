// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"
)

// UIDesktopClosedStore shares the desktop sessions their person closed across replicas and restarts
// (internal/api/uigateway_desktop.go). key is a one-way digest of the session's relay key, never
// principal text.
type UIDesktopClosedStore interface {
	// CloseUIDesktop upserts key's Close, held until expiresAt.
	CloseUIDesktop(ctx context.Context, key string, expiresAt time.Time) error
	// UIDesktopClosed reports whether key holds a Close that has not expired at now.
	UIDesktopClosed(ctx context.Context, key string, now time.Time) (bool, error)
	// PruneUIDesktopClosed deletes the Closes expired at now, reporting the count removed.
	PruneUIDesktopClosed(ctx context.Context, now time.Time) (int, error)
}

var _ UIDesktopClosedStore = PG{}

// CloseUIDesktop implements UIDesktopClosedStore.
func (s PG) CloseUIDesktop(ctx context.Context, key string, expiresAt time.Time) error {
	const q = `INSERT INTO ui_desktop_closed (key_digest, expires_at) VALUES ($1, $2)
		ON CONFLICT (key_digest) DO UPDATE SET expires_at = EXCLUDED.expires_at`
	if _, err := s.Pool.Exec(ctx, q, key, expiresAt); err != nil {
		return fmt.Errorf("store: close ui desktop: %w", err)
	}
	return nil
}

// UIDesktopClosed implements UIDesktopClosedStore.
func (s PG) UIDesktopClosed(ctx context.Context, key string, now time.Time) (bool, error) {
	var closed bool
	err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM ui_desktop_closed WHERE key_digest = $1 AND expires_at > $2)`, key, now).Scan(&closed)
	if err != nil {
		return false, fmt.Errorf("store: read ui desktop closed: %w", err)
	}
	return closed, nil
}

// PruneUIDesktopClosed implements UIDesktopClosedStore
// (ui_desktop_closed_expires_at_idx backs the WHERE clause).
func (s PG) PruneUIDesktopClosed(ctx context.Context, now time.Time) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM ui_desktop_closed WHERE expires_at <= $1`, now)
	if err != nil {
		return 0, fmt.Errorf("store: prune ui desktop closed: %w", err)
	}
	return int(tag.RowsAffected()), nil
}
