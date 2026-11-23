// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// OutageWindow is one recorded window. EndedAt is nil while it is open.
type OutageWindow struct {
	ID        int64
	Kind      string
	RunnerID  *uuid.UUID
	StartedAt time.Time
	EndedAt   *time.Time
}

// The window kinds.
const (
	OutageKindControlPlane = "cp_outage"
	OutageKindRunnerLink   = "runner_link"
	OutageKindServer       = "server_outage"
)

const (
	// OutageCoverLead is how long before a token's expiry a window must have begun to cover it. A proxy renews at
	// most 15 minutes apart (egress/proxy renew.go renewMaxInterval), so a token that was being renewed cannot expire
	// within 15 minutes of a fault's start.
	OutageCoverLead = 15 * time.Minute
	// OutageCoverSlack is how far after a window's end a covered expiry may lie: the stamp lands a moment after the
	// mint, and a replica's clock may differ from the database's by a little (identity.RunTokenLapseAfter's margin).
	OutageCoverSlack = 5 * time.Minute
	// RunnerLinkWindowMax closes a runner_link window whose runner never came back: a day past any recoverable
	// token (part 2's 24 h bound) plus an hour.
	RunnerLinkWindowMax = 25 * time.Hour
	// ControlPlaneGapOver is the longest gap between two heartbeats that is not an outage. A longer gap becomes a
	// cp_outage window when beating resumes.
	ControlPlaneGapOver = 30 * time.Second
)

// OutageWindows records and reads outage windows. Every writer is the control plane's own observation.
type OutageWindows interface {
	BeatControlPlane(ctx context.Context, gapOver time.Duration) (*OutageWindow, error)
	OpenRunnerLinkWindow(ctx context.Context, runnerID uuid.UUID, sessionID string, droppedFor time.Duration) (bool, error)
	CloseRunnerLinkWindow(ctx context.Context, runnerID uuid.UUID) error
	HealRunnerLinkWindows(ctx context.Context) (int, error)
	OpenServerOutageWindow(ctx context.Context, since time.Duration, detail map[string]any) error
	CloseServerOutageWindow(ctx context.Context) error
	PruneOutageWindows(ctx context.Context, olderThan time.Duration) (int, error)
	OutageWindowCovers(ctx context.Context, runnerID *uuid.UUID, placement types.Placement, expiry time.Time) (OutageWindow, bool, error)
}

var _ OutageWindows = PG{}

// OutageCoversSQL is the one rule for "a window w covers a token expiring at expiry for a run on runner, with
// placement": an expression over outage_windows alias w, with each argument a SQL expression (a placeholder or a
// column). A server run is one with no runner that is not local (a deleted runner's local run keeps its placement).
// OutageWindowCovers and any query that must agree with it (the recovery hold) build on it.
func OutageCoversSQL(expiry, runner, placement string) string {
	return fmt.Sprintf(`w.started_at <= (%[1]s) - interval '%[4]d seconds'
	  AND COALESCE(w.ended_at, now()) >= (%[1]s) - interval '%[5]d seconds'
	  AND (w.kind = 'cp_outage'
	       OR (w.kind = 'server_outage' AND (%[2]s) IS NULL AND (%[3]s) <> 'local')
	       OR (w.kind = 'runner_link' AND w.runner_id = (%[2]s)))`,
		expiry, runner, placement, int(OutageCoverLead/time.Second), int(OutageCoverSlack/time.Second))
}

// BeatControlPlane records one heartbeat on the database's clock and, when the previous beat is more than gapOver
// old, writes the gap as a closed cp_outage window and returns it. The row lock makes two replicas beating at once
// write one window per gap: the second reads the first's fresh beat.
func (s PG) BeatControlPlane(ctx context.Context, gapOver time.Duration) (*OutageWindow, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return nil, fmt.Errorf("store: begin heartbeat tx: %w", err)
	}
	defer tx.Rollback(ctx) //nolint:errcheck // a committed transaction is closed
	var prev time.Time
	err = tx.QueryRow(ctx, `SELECT beat_at FROM cp_heartbeat WHERE singleton FOR UPDATE`).Scan(&prev)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err := tx.Exec(ctx, `INSERT INTO cp_heartbeat (singleton, beat_at) VALUES (true, clock_timestamp()) ON CONFLICT DO NOTHING`); err != nil {
			return nil, fmt.Errorf("store: first heartbeat: %w", err)
		}
		return nil, tx.Commit(ctx)
	}
	if err != nil {
		return nil, fmt.Errorf("store: read heartbeat: %w", err)
	}
	var now time.Time
	if err := tx.QueryRow(ctx, `UPDATE cp_heartbeat SET beat_at = clock_timestamp() WHERE singleton RETURNING beat_at`).Scan(&now); err != nil {
		return nil, fmt.Errorf("store: heartbeat: %w", err)
	}
	var w *OutageWindow
	if now.Sub(prev) > gapOver {
		w = &OutageWindow{Kind: OutageKindControlPlane, StartedAt: prev, EndedAt: &now}
		if err := tx.QueryRow(ctx, `INSERT INTO outage_windows (kind, started_at, ended_at, detail) VALUES ('cp_outage',
			$1, $2, jsonb_build_object('gap_s', EXTRACT(EPOCH FROM $2::timestamptz - $1::timestamptz)::bigint)) RETURNING id`,
			prev, now).Scan(&w.ID); err != nil {
			return nil, fmt.Errorf("store: record control-plane outage: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("store: commit heartbeat: %w", err)
	}
	return w, nil
}

// OpenRunnerLinkWindow opens runnerID's runner_link window, starting droppedFor before the database's now, and
// reports whether it inserted one. It inserts only while runner_inventories still holds sessionID for that runner,
// read under that row's lock: a fresh session's BeginRunnerInventory takes the same row FOR UPDATE and replaces it.
// If that session holds the lock first, this statement waits, then finds no row with the old id (READ COMMITTED
// re-checks the locked row) and inserts nothing. If this statement holds the lock first, its insert commits before
// that session can report Connected, which closes the window. An open window for the runner makes it a no-op.
func (s PG) OpenRunnerLinkWindow(ctx context.Context, runnerID uuid.UUID, sessionID string, droppedFor time.Duration) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `WITH cur AS (
    SELECT runner_id FROM runner_inventories
     WHERE runner_id = $1 AND session_id::text = $3::text
       FOR UPDATE)
INSERT INTO outage_windows (kind, runner_id, started_at)
SELECT 'runner_link', cur.runner_id, now() - $2::interval FROM cur
ON CONFLICT DO NOTHING`, runnerID, droppedFor.String(), sessionID)
	if err != nil {
		return false, fmt.Errorf("store: open runner link window: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// CloseRunnerLinkWindow ends runnerID's open runner_link window, if it has one, at the database's now.
func (s PG) CloseRunnerLinkWindow(ctx context.Context, runnerID uuid.UUID) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE outage_windows SET ended_at = greatest(started_at, now()) WHERE kind = 'runner_link' AND runner_id = $1 AND ended_at IS NULL`,
		runnerID); err != nil {
		return fmt.Errorf("store: close runner link window: %w", err)
	}
	return nil
}

// HealRunnerLinkWindows closes every open runner_link window whose runner was deleted, or that has been open
// longer than RunnerLinkWindowMax, and returns how many it closed.
func (s PG) HealRunnerLinkWindows(ctx context.Context) (int, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE outage_windows w SET ended_at = greatest(w.started_at, now()) WHERE w.kind = 'runner_link' AND w.ended_at IS NULL
   AND (NOT EXISTS (SELECT 1 FROM runners r WHERE r.id = w.runner_id) OR w.started_at < now() - $1::interval)`, RunnerLinkWindowMax.String())
	if err != nil {
		return 0, fmt.Errorf("store: heal runner link windows: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// OpenServerOutageWindow opens the server_outage window, starting since before the database's now, with detail
// naming why. An open server_outage window makes it a no-op.
func (s PG) OpenServerOutageWindow(ctx context.Context, since time.Duration, detail map[string]any) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO outage_windows (kind, started_at, detail) VALUES ('server_outage', now() - $1::interval, $2) ON CONFLICT DO NOTHING`,
		since.String(), detail); err != nil {
		return fmt.Errorf("store: open server outage window: %w", err)
	}
	return nil
}

// CloseServerOutageWindow ends the open server_outage window, if there is one, at the database's now.
func (s PG) CloseServerOutageWindow(ctx context.Context) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE outage_windows SET ended_at = greatest(started_at, now()) WHERE kind = 'server_outage' AND ended_at IS NULL`); err != nil {
		return fmt.Errorf("store: close server outage window: %w", err)
	}
	return nil
}

// PruneOutageWindows deletes the windows that ended more than olderThan ago and returns how many it deleted.
func (s PG) PruneOutageWindows(ctx context.Context, olderThan time.Duration) (int, error) {
	tag, err := s.Pool.Exec(ctx, `DELETE FROM outage_windows WHERE ended_at IS NOT NULL AND ended_at < now() - $1::interval`, olderThan.String())
	if err != nil {
		return 0, fmt.Errorf("store: prune outage windows: %w", err)
	}
	return int(tag.RowsAffected()), nil
}

// OutageWindowCovers returns the window that covers a token expiring at expiry for a run on runnerID (nil for
// none) with placement, by OutageCoversSQL: the one that ends latest, an open one first. ok is false when none does.
func (s PG) OutageWindowCovers(ctx context.Context, runnerID *uuid.UUID, placement types.Placement, expiry time.Time) (OutageWindow, bool, error) {
	q := `SELECT w.id, w.kind, w.runner_id, w.started_at, w.ended_at FROM outage_windows w WHERE ` +
		OutageCoversSQL("$2::timestamptz", "$1::uuid", "$3::text") +
		` ORDER BY COALESCE(w.ended_at, 'infinity'::timestamptz) DESC, w.started_at DESC LIMIT 1`
	var w OutageWindow
	err := s.Pool.QueryRow(ctx, q, runnerID, expiry, string(placement)).Scan(&w.ID, &w.Kind, &w.RunnerID, &w.StartedAt, &w.EndedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return OutageWindow{}, false, nil
	}
	if err != nil {
		return OutageWindow{}, false, fmt.Errorf("store: read outage windows: %w", err)
	}
	return w, true, nil
}
