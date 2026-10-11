// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5"
)

// RowQuerier is the clock-read surface shared by pools, acquired connections and transactions.
type RowQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

// AppClockAnchor pairs a database clock reading with the app reading taken after
// its successful receipt. Response uncertainty moves translated instants earlier,
// toward revocation; waits after capture cannot move a fixed instant forward.
type AppClockAnchor struct {
	DatabaseAt time.Time
	AppAt      time.Time
}

// CaptureAppClock samples the app clock only after the database read succeeds.
// A caller holding a transaction or identity guard keeps that same query surface.
func CaptureAppClock(ctx context.Context, q RowQuerier, appNow func() time.Time) (AppClockAnchor, error) {
	var at time.Time
	if err := q.QueryRow(ctx, `SELECT clock_timestamp()`).Scan(&at); err != nil {
		return AppClockAnchor{}, err
	}
	return AppClockAnchor{DatabaseAt: at, AppAt: appNow()}, nil
}

// Translate maps an app instant to a fixed database instant using the existing
// bounded, nonnegative age. A future instant maps to DatabaseAt, never after it.
func (a AppClockAnchor) Translate(at time.Time) time.Time {
	return a.DatabaseAt.Add(-time.Duration(AppClockAgeMicros(at, a.AppAt)) * time.Microsecond)
}
