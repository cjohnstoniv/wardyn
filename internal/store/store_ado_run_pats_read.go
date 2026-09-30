// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The console's reads of ado_run_pats (#1428): every token a run held, and the
// newest token created for a person on a row. Like the table, neither carries
// a secret.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// RunPATReader reads the run-token record for display. Optional like
// RunPATStore: a store without it has no tokens to show.
type RunPATReader interface {
	// ListRunPATs returns every token recorded for runID, revoked or not,
	// oldest first. Empty, never nil, when there is none.
	ListRunPATs(ctx context.Context, runID uuid.UUID) ([]RunPAT, error)
	// LastRunPAT returns the newest token recorded for owner on
	// providerRowID; found=false when there is none.
	LastRunPAT(ctx context.Context, owner, providerRowID string) (p RunPAT, found bool, err error)
}

var (
	_ RunPATReader = PG{}
	_ RunPATReader = (*MemRunPATs)(nil)
)

func scanRunPAT(row pgx.Row) (RunPAT, error) {
	var p RunPAT
	err := row.Scan(&p.RunID, &p.AuthorizationID, &p.Owner, &p.ProviderRowID, &p.Org, &p.Scope,
		&p.ValidTo, &p.CreatedAt, &p.RevokedAt, &p.RevokeReason, &p.LastError)
	return p, err
}

// ListRunPATs — see RunPATReader. The primary key's run_id prefix serves it.
func (s PG) ListRunPATs(ctx context.Context, runID uuid.UUID) ([]RunPAT, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+runPATCols+` FROM ado_run_pats
		WHERE run_id = $1 ORDER BY created_at, authorization_id`, runID)
	if err != nil {
		return nil, fmt.Errorf("store: list run pats: %w", err)
	}
	defer rows.Close()
	out := []RunPAT{}
	for rows.Next() {
		p, err := scanRunPAT(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan run pat: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list run pats: %w", err)
	}
	return out, nil
}

// LastRunPAT — see RunPATReader.
func (s PG) LastRunPAT(ctx context.Context, owner, providerRowID string) (RunPAT, bool, error) {
	p, err := scanRunPAT(s.Pool.QueryRow(ctx, `SELECT `+runPATCols+` FROM ado_run_pats
		WHERE owner = $1 AND provider_row_id = $2 ORDER BY created_at DESC, authorization_id DESC LIMIT 1`,
		owner, providerRowID))
	if errors.Is(err, pgx.ErrNoRows) {
		return RunPAT{}, false, nil
	}
	if err != nil {
		return RunPAT{}, false, fmt.Errorf("store: last run pat: %w", err)
	}
	return p, true, nil
}

func (m *MemRunPATs) ListRunPATs(_ context.Context, runID uuid.UUID) ([]RunPAT, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []RunPAT{}
	for _, r := range m.rows {
		if r.RunID == runID {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, compareRunPATAge)
	return out, nil
}

func (m *MemRunPATs) LastRunPAT(_ context.Context, owner, providerRowID string) (RunPAT, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var last RunPAT
	found := false
	for _, r := range m.rows {
		if r.Owner == owner && r.ProviderRowID == providerRowID && (!found || compareRunPATAge(r, last) > 0) {
			last, found = r, true
		}
	}
	return last, found, nil
}

// compareRunPATAge orders tokens as the PG queries do: created_at, then
// authorization id.
func compareRunPATAge(a, b RunPAT) int {
	if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
		return c
	}
	return slices.Compare(a.AuthorizationID[:], b.AuthorizationID[:])
}
