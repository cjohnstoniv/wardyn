// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Run previews: a preview URL on a port a run serves, and who may open it. Every time is written and compared
// by the database's clock, so a replica's clock decides nothing about access. Nothing here writes agent_runs:
// a preview is never a run's activity.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// runPreviewCols is every column of a preview but its link hash, in scanRunPreview's order.
const runPreviewCols = `id, run_id, port, visibility, created_by, created_at, updated_at, expires_at, revoked_at`

// runPreviewLive narrows to the rows a preview may be opened by: not revoked and not expired.
const runPreviewLive = `revoked_at IS NULL AND expires_at > now()`

func scanRunPreview(row interface{ Scan(...any) error }) (types.RunPreview, error) {
	var p types.RunPreview
	var vis string
	err := row.Scan(&p.ID, &p.RunID, &p.Port, &vis, &p.CreatedBy, &p.CreatedAt, &p.UpdatedAt, &p.ExpiresAt, &p.RevokedAt)
	p.Visibility = types.PreviewVisibility(vis)
	return p, err
}

func (s PG) oneRunPreview(ctx context.Context, what, sql string, args ...any) (types.RunPreview, error) {
	p, err := scanRunPreview(s.Pool.QueryRow(ctx, sql, args...))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.RunPreview{}, ErrNotFound
	}
	if err != nil {
		return types.RunPreview{}, fmt.Errorf("store: %s: %w", what, err)
	}
	return p, nil
}

func (s PG) manyRunPreviews(ctx context.Context, what, sql string, args ...any) ([]types.RunPreview, error) {
	rows, err := s.Pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	out, err := pgx.CollectRows(rows, func(row pgx.CollectableRow) (types.RunPreview, error) { return scanRunPreview(row) })
	if err != nil {
		return nil, fmt.Errorf("store: %s: %w", what, err)
	}
	return out, nil
}

// UpsertRunPreview creates or changes the preview of (runID, port) in one transaction. When the port's live row
// is unexpired and already has vis, that row keeps its id and its link hash, expires_at becomes now() + ttl,
// linkHash is not stored, and created is false. Otherwise the port's unrevoked row, an expired one included, is
// revoked by by, and a new row with a new id, vis, linkHash and expires_at = now() + ttl is inserted; created
// is true. So a link hash is only ever stored on a new row. linkHash is set exactly when vis is public.
func (s PG) UpsertRunPreview(ctx context.Context, runID uuid.UUID, port int, vis types.PreviewVisibility, ttl time.Duration, by string, linkHash []byte) (p types.RunPreview, created bool, err error) {
	p, created, err = s.upsertRunPreview(ctx, runID, port, vis, ttl, by, linkHash)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" && pgErr.ConstraintName == "run_previews_one_live" {
		// Another write inserted the port's first row after this one looked: run once more, which sees it.
		p, created, err = s.upsertRunPreview(ctx, runID, port, vis, ttl, by, linkHash)
	}
	return p, created, err
}

func (s PG) upsertRunPreview(ctx context.Context, runID uuid.UUID, port int, vis types.PreviewVisibility, ttl time.Duration, by string, linkHash []byte) (types.RunPreview, bool, error) {
	tx, err := s.Pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return types.RunPreview{}, false, fmt.Errorf("store: upsert a run preview: %w", err)
	}
	defer db.Rollback(ctx, tx) // a no-op after Commit
	var (
		id      uuid.UUID
		was     string
		current bool
	)
	err = tx.QueryRow(ctx, `SELECT id, visibility, expires_at > now() FROM run_previews
		WHERE run_id = $1 AND port = $2 AND revoked_at IS NULL FOR UPDATE`, runID, port).Scan(&id, &was, &current)
	found := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return types.RunPreview{}, false, fmt.Errorf("store: read a run preview: %w", err)
	}
	micros := ttl.Microseconds()
	var out types.RunPreview
	created := true
	switch {
	case found && current && was == string(vis):
		created = false
		out, err = scanRunPreview(tx.QueryRow(ctx, `UPDATE run_previews
			SET expires_at = now() + $2::bigint * interval '1 microsecond', updated_at = now()
			WHERE id = $1 RETURNING `+runPreviewCols, id, micros))
	default:
		if found {
			if _, err := tx.Exec(ctx, `UPDATE run_previews SET revoked_at = now(), revoked_by = $2, updated_at = now()
				WHERE id = $1`, id, by); err != nil {
				return types.RunPreview{}, false, fmt.Errorf("store: revoke a run preview: %w", err)
			}
		}
		out, err = scanRunPreview(tx.QueryRow(ctx, `INSERT INTO run_previews
				(id, run_id, port, visibility, link_hash, created_by, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, now() + $7::bigint * interval '1 microsecond')
			RETURNING `+runPreviewCols, uuid.New(), runID, port, string(vis), linkHash, by, micros))
	}
	if err != nil {
		return types.RunPreview{}, false, fmt.Errorf("store: upsert a run preview: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return types.RunPreview{}, false, fmt.Errorf("store: upsert a run preview: %w", err)
	}
	return out, created, nil
}

// ListRunPreviews lists the run's live previews, by port.
func (s PG) ListRunPreviews(ctx context.Context, runID uuid.UUID) ([]types.RunPreview, error) {
	return s.manyRunPreviews(ctx, "list run previews", `SELECT `+runPreviewCols+` FROM run_previews
		WHERE run_id = $1 AND `+runPreviewLive+` ORDER BY port`, runID)
}

// LiveRunPreview reads the live preview of (runID, port); ErrNotFound when there is none.
func (s PG) LiveRunPreview(ctx context.Context, runID uuid.UUID, port int) (types.RunPreview, error) {
	return s.oneRunPreview(ctx, "read a run preview", `SELECT `+runPreviewCols+` FROM run_previews
		WHERE run_id = $1 AND port = $2 AND `+runPreviewLive, runID, port)
}

// RunPreviewByLink reads the live public preview whose link hash is hash; ErrNotFound when there is none.
func (s PG) RunPreviewByLink(ctx context.Context, hash []byte) (types.RunPreview, error) {
	return s.oneRunPreview(ctx, "read a run preview by link", `SELECT `+runPreviewCols+` FROM run_previews
		WHERE link_hash = $1 AND visibility = 'public' AND `+runPreviewLive, hash)
}

// RevokeRunPreview revokes the unrevoked preview of (runID, port), an expired one included, by by, and returns
// it; ErrNotFound when there is none.
func (s PG) RevokeRunPreview(ctx context.Context, runID uuid.UUID, port int, by string) (types.RunPreview, error) {
	return s.oneRunPreview(ctx, "revoke a run preview", `UPDATE run_previews
		SET revoked_at = now(), revoked_by = $3, updated_at = now()
		WHERE run_id = $1 AND port = $2 AND revoked_at IS NULL
		RETURNING `+runPreviewCols, runID, port, by)
}

// RevokeRunPreviewsAbove revokes, by by, every unrevoked preview of any run whose visibility is wider than
// ceiling, and returns them. A ceiling that is not a visibility (the empty one included) revokes them all.
func (s PG) RevokeRunPreviewsAbove(ctx context.Context, ceiling types.PreviewVisibility, by string) ([]types.RunPreview, error) {
	keep := []string{}
	for _, v := range []types.PreviewVisibility{types.PreviewPrivate, types.PreviewOrg, types.PreviewPublic} {
		if v.AtMost(ceiling) {
			keep = append(keep, string(v))
		}
	}
	return s.manyRunPreviews(ctx, "revoke run previews above a ceiling", `UPDATE run_previews
		SET revoked_at = now(), revoked_by = $2, updated_at = now()
		WHERE revoked_at IS NULL AND NOT (visibility = ANY($1::text[]))
		RETURNING `+runPreviewCols, keep, by)
}
