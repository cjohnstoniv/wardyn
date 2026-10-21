// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// DesktopSigninStore keeps desktop sign-ins and their shares (migration 0212).
type DesktopSigninStore interface {
	// BeginDesktopSignin inserts an open sign-in, or ErrConflict when the run already has one.
	BeginDesktopSignin(ctx context.Context, s types.DesktopSignin) error
	// SetDesktopSigninPauseRecord stores pause-agent's output on an open sign-in.
	SetDesktopSigninPauseRecord(ctx context.Context, id uuid.UUID, rec json.RawMessage) error
	// OpenDesktopSignin is the run's open sign-in, or ErrNotFound.
	OpenDesktopSignin(ctx context.Context, runID uuid.UUID) (types.DesktopSignin, error)
	// ListOpenDesktopSignins is every open sign-in, for the sweep.
	ListOpenDesktopSignins(ctx context.Context) ([]types.DesktopSignin, error)
	// DesktopSigninCredentialOpen reports whether hash is the credential of
	// the run's open, unexpired sign-in.
	DesktopSigninCredentialOpen(ctx context.Context, runID uuid.UUID, hash []byte, now time.Time) (bool, error)
	// RequestDesktopSigninEnd marks an open sign-in for the sweep to end with
	// reason (the first reason stays) and records the stage the end is stuck
	// at; true when that changed the row's state.
	RequestDesktopSigninEnd(ctx context.Context, id uuid.UUID, reason, stage string) (bool, error)
	// EndDesktopSignin closes an open sign-in; false when it was not open.
	EndDesktopSignin(ctx context.Context, id uuid.UUID, reason string, at time.Time) (bool, error)
	// RecordDesktopSigninShare inserts one share of an open sign-in.
	RecordDesktopSigninShare(ctx context.Context, sh types.DesktopSigninShare) error
	// CountDesktopSigninShares is how many sites a sign-in shared.
	CountDesktopSigninShares(ctx context.Context, signinID uuid.UUID) (int, error)
	// RunHadDesktopSignin reports whether any sign-in ever began on the run:
	// a begun sign-in is a non-empty store, since the sandbox cannot be
	// trusted to report it empty.
	RunHadDesktopSignin(ctx context.Context, runID uuid.UUID) (bool, error)
	// RunsWithOpenDesktopSignin is the set of runs with an open sign-in.
	RunsWithOpenDesktopSignin(ctx context.Context) (map[uuid.UUID]bool, error)
	// MarkDesktopSigninRun records dispatch's sign-in decision for a desktop
	// run: offReason "" gave it sign-ins, any other value names why not.
	MarkDesktopSigninRun(ctx context.Context, runID uuid.UUID, offReason string) error
	// DesktopSigninRun is that decision; found is false for a run dispatch
	// decided nothing for (no desktop, sign-ins off, or another runner).
	DesktopSigninRun(ctx context.Context, runID uuid.UUID) (offReason string, found bool, err error)
}

var _ DesktopSigninStore = PG{}

const desktopSigninCols = `id, run_id, holder, credential_hash, pause_record, began_at, expires_at,
	COALESCE(end_requested, ''), COALESCE(end_stage, ''), ended_at, COALESCE(end_reason, '')`

func scanDesktopSignin(row pgx.Row) (types.DesktopSignin, error) {
	var s types.DesktopSignin
	var rec []byte
	err := row.Scan(&s.ID, &s.RunID, &s.Holder, &s.CredentialHash, &rec, &s.BeganAt, &s.ExpiresAt,
		&s.EndRequested, &s.EndStage, &s.EndedAt, &s.EndReason)
	if len(rec) > 0 {
		s.PauseRecord = rec
	}
	return s, err
}

// BeginDesktopSignin — see DesktopSigninStore.
func (s PG) BeginDesktopSignin(ctx context.Context, si types.DesktopSignin) error {
	_, err := s.Pool.Exec(ctx, `INSERT INTO desktop_signins (id, run_id, holder, credential_hash, began_at, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)`, si.ID, si.RunID, si.Holder, si.CredentialHash, si.BeganAt, si.ExpiresAt)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	if err != nil {
		return fmt.Errorf("store: begin desktop sign-in: %w", err)
	}
	return nil
}

// SetDesktopSigninPauseRecord — see DesktopSigninStore.
func (s PG) SetDesktopSigninPauseRecord(ctx context.Context, id uuid.UUID, rec json.RawMessage) error {
	if _, err := s.Pool.Exec(ctx, `UPDATE desktop_signins SET pause_record = $2 WHERE id = $1 AND ended_at IS NULL`, id, []byte(rec)); err != nil {
		return fmt.Errorf("store: desktop sign-in pause record: %w", err)
	}
	return nil
}

// OpenDesktopSignin — see DesktopSigninStore.
func (s PG) OpenDesktopSignin(ctx context.Context, runID uuid.UUID) (types.DesktopSignin, error) {
	si, err := scanDesktopSignin(s.Pool.QueryRow(ctx, `SELECT `+desktopSigninCols+`
		FROM desktop_signins WHERE run_id = $1 AND ended_at IS NULL`, runID))
	if errors.Is(err, pgx.ErrNoRows) {
		return types.DesktopSignin{}, ErrNotFound
	}
	if err != nil {
		return types.DesktopSignin{}, fmt.Errorf("store: open desktop sign-in: %w", err)
	}
	return si, nil
}

// ListOpenDesktopSignins — see DesktopSigninStore.
func (s PG) ListOpenDesktopSignins(ctx context.Context) ([]types.DesktopSignin, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+desktopSigninCols+` FROM desktop_signins WHERE ended_at IS NULL ORDER BY began_at`)
	if err != nil {
		return nil, fmt.Errorf("store: list open desktop sign-ins: %w", err)
	}
	defer rows.Close()
	var out []types.DesktopSignin
	for rows.Next() {
		si, err := scanDesktopSignin(rows)
		if err != nil {
			return nil, fmt.Errorf("store: list open desktop sign-ins: %w", err)
		}
		out = append(out, si)
	}
	return out, rows.Err()
}

// DesktopSigninCredentialOpen — see DesktopSigninStore.
func (s PG) DesktopSigninCredentialOpen(ctx context.Context, runID uuid.UUID, hash []byte, now time.Time) (bool, error) {
	var stored []byte
	err := s.Pool.QueryRow(ctx, `SELECT credential_hash FROM desktop_signins
		WHERE run_id = $1 AND ended_at IS NULL AND end_requested IS NULL AND expires_at > $2`, runID, now).Scan(&stored)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store: desktop sign-in credential: %w", err)
	}
	return len(hash) > 0 && subtle.ConstantTimeCompare(stored, hash) == 1, nil
}

// RequestDesktopSigninEnd — see DesktopSigninStore.
func (s PG) RequestDesktopSigninEnd(ctx context.Context, id uuid.UUID, reason, stage string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE desktop_signins SET end_requested = COALESCE(end_requested, $2), end_stage = $3
		WHERE id = $1 AND ended_at IS NULL AND (end_requested IS NULL OR end_stage IS DISTINCT FROM $3)`, id, reason, stage)
	if err != nil {
		return false, fmt.Errorf("store: request desktop sign-in end: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// EndDesktopSignin — see DesktopSigninStore.
func (s PG) EndDesktopSignin(ctx context.Context, id uuid.UUID, reason string, at time.Time) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `UPDATE desktop_signins SET ended_at = $3, end_reason = $2
		WHERE id = $1 AND ended_at IS NULL`, id, reason, at)
	if err != nil {
		return false, fmt.Errorf("store: end desktop sign-in: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// RecordDesktopSigninShare — see DesktopSigninStore. The sign-in must still be open.
func (s PG) RecordDesktopSigninShare(ctx context.Context, sh types.DesktopSigninShare) error {
	tag, err := s.Pool.Exec(ctx, `INSERT INTO desktop_signin_shares (id, signin_id, run_id, site, cookie_count, shared_by, shared_at)
		SELECT $1, d.id, d.run_id, $3, $4, $5, $6 FROM desktop_signins d
		WHERE d.id = $2 AND d.run_id = $7 AND d.ended_at IS NULL`,
		sh.ID, sh.SigninID, sh.Site, sh.CookieCount, sh.SharedBy, sh.SharedAt, sh.RunID)
	if err != nil {
		return fmt.Errorf("store: record desktop sign-in share: %w", err)
	}
	if tag.RowsAffected() != 1 {
		return ErrNotFound
	}
	return nil
}

// CountDesktopSigninShares — see DesktopSigninStore.
func (s PG) CountDesktopSigninShares(ctx context.Context, signinID uuid.UUID) (int, error) {
	var n int
	if err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM desktop_signin_shares WHERE signin_id = $1`, signinID).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count desktop sign-in shares: %w", err)
	}
	return n, nil
}

// RunHadDesktopSignin — see DesktopSigninStore.
func (s PG) RunHadDesktopSignin(ctx context.Context, runID uuid.UUID) (bool, error) {
	var had bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM desktop_signins WHERE run_id = $1)`, runID).Scan(&had); err != nil {
		return false, fmt.Errorf("store: run had desktop sign-in: %w", err)
	}
	return had, nil
}

// RunsWithOpenDesktopSignin — see DesktopSigninStore.
func (s PG) RunsWithOpenDesktopSignin(ctx context.Context) (map[uuid.UUID]bool, error) {
	rows, err := s.Pool.Query(ctx, `SELECT run_id FROM desktop_signins WHERE ended_at IS NULL`)
	if err != nil {
		return nil, fmt.Errorf("store: runs with an open desktop sign-in: %w", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MarkDesktopSigninRun — see DesktopSigninStore.
func (s PG) MarkDesktopSigninRun(ctx context.Context, runID uuid.UUID, offReason string) error {
	if _, err := s.Pool.Exec(ctx, `INSERT INTO desktop_signin_runs (run_id, off_reason) VALUES ($1, $2)
		ON CONFLICT (run_id) DO UPDATE SET off_reason = EXCLUDED.off_reason`, runID, offReason); err != nil {
		return fmt.Errorf("store: mark desktop sign-in run: %w", err)
	}
	return nil
}

// DesktopSigninRun — see DesktopSigninStore.
func (s PG) DesktopSigninRun(ctx context.Context, runID uuid.UUID) (string, bool, error) {
	var reason string
	err := s.Pool.QueryRow(ctx, `SELECT off_reason FROM desktop_signin_runs WHERE run_id = $1`, runID).Scan(&reason)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("store: desktop sign-in run: %w", err)
	}
	return reason, true, nil
}
