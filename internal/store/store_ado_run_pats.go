// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The Azure DevOps personal access tokens Wardyn creates for `minted_pat` runs
// (migration 0101, #1428): the record that makes a token revocable after a
// crash. A row holds no secret; the token value lives in daemon memory only.
package store

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"
)

// RunPAT is one row of ado_run_pats.
type RunPAT struct {
	RunID           uuid.UUID
	AuthorizationID uuid.UUID
	Owner           string // whose token: the run's creator
	ProviderRowID   string // the provider row the run was dispatched on
	Org             string // the Azure DevOps organisation the token is scoped to
	Scope           string // the space-joined scope string it was created with
	ValidTo         time.Time
	CreatedAt       time.Time  // stamped by the store on insert; ignored on the way in
	RevokedAt       *time.Time // nil while Wardyn believes the token is live
	RevokeReason    string
	LastError       string // why a revoke was abandoned; "" otherwise
}

// RunPATFilter narrows ListUnrevokedRunPATs; a zero field matches everything.
type RunPATFilter struct {
	RunID         uuid.UUID
	Owner         string
	ProviderRowID string
}

func (f RunPATFilter) matches(p RunPAT) bool {
	return (f.RunID == uuid.Nil || f.RunID == p.RunID) &&
		(f.Owner == "" || f.Owner == p.Owner) &&
		(f.ProviderRowID == "" || f.ProviderRowID == p.ProviderRowID)
}

// RunPATStore is the run-token record. Optional like RunPauser: it is not part
// of Store because the test doubles that embed Store would route these to a nil
// interface; the api layer type-asserts. Production is always PG; MemRunPATs is
// the same contract without a database.
type RunPATStore interface {
	// InsertRunPAT records a token BEFORE its first use. ErrConflict when that
	// (run, authorization id) is already recorded. CreatedAt is the store's.
	InsertRunPAT(ctx context.Context, p RunPAT) error
	// MarkRunPATRevoked closes a token's record with reason, and reports
	// whether it did: false when the row is unknown or already closed, so two
	// revokers race harmlessly. lastError is non-empty only when the revoke was
	// abandoned (the token then expires on its own at ValidTo).
	MarkRunPATRevoked(ctx context.Context, runID, authorizationID uuid.UUID, reason, lastError string) (bool, error)
	// ListUnrevokedRunPATs returns the tokens still recorded live that match f,
	// oldest first.
	ListUnrevokedRunPATs(ctx context.Context, f RunPATFilter) ([]RunPAT, error)
}

var (
	_ RunPATStore = PG{}
	_ RunPATStore = (*MemRunPATs)(nil)
)

func (p RunPAT) validate() error {
	switch {
	case p.RunID == uuid.Nil || p.AuthorizationID == uuid.Nil:
		return errors.New("store: InsertRunPAT: run id and authorization id are required")
	case p.Owner == "" || p.ProviderRowID == "" || p.Org == "" || p.Scope == "":
		return errors.New("store: InsertRunPAT: owner, provider row, org and scope are required")
	case p.ValidTo.IsZero():
		return errors.New("store: InsertRunPAT: valid_to is required")
	}
	return nil
}

const runPATCols = `run_id, authorization_id, owner, provider_row_id, org, scope, valid_to, created_at, revoked_at, revoke_reason, last_error`

// InsertRunPAT — see RunPATStore.
func (s PG) InsertRunPAT(ctx context.Context, p RunPAT) error {
	if err := p.validate(); err != nil {
		return err
	}
	_, err := s.Pool.Exec(ctx, `
		INSERT INTO ado_run_pats (run_id, authorization_id, owner, provider_row_id, org, scope, valid_to)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		p.RunID, p.AuthorizationID, p.Owner, p.ProviderRowID, p.Org, p.Scope, p.ValidTo)
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return fmt.Errorf("%w: run token already recorded", ErrConflict)
	}
	if err != nil {
		return fmt.Errorf("store: insert run pat: %w", err)
	}
	return nil
}

// MarkRunPATRevoked — see RunPATStore.
func (s PG) MarkRunPATRevoked(ctx context.Context, runID, authorizationID uuid.UUID, reason, lastError string) (bool, error) {
	tag, err := s.Pool.Exec(ctx, `
		UPDATE ado_run_pats SET revoked_at = now(), revoke_reason = $3, last_error = $4
		WHERE run_id = $1 AND authorization_id = $2 AND revoked_at IS NULL`,
		runID, authorizationID, reason, lastError)
	if err != nil {
		return false, fmt.Errorf("store: mark run pat revoked: %w", err)
	}
	return tag.RowsAffected() == 1, nil
}

// ListUnrevokedRunPATs — see RunPATStore.
func (s PG) ListUnrevokedRunPATs(ctx context.Context, f RunPATFilter) ([]RunPAT, error) {
	var runID *uuid.UUID
	if f.RunID != uuid.Nil {
		runID = &f.RunID
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT `+runPATCols+` FROM ado_run_pats
		WHERE revoked_at IS NULL
		  AND ($1::uuid IS NULL OR run_id = $1)
		  AND ($2 = '' OR owner = $2)
		  AND ($3 = '' OR provider_row_id = $3)
		ORDER BY created_at, authorization_id`, runID, f.Owner, f.ProviderRowID)
	if err != nil {
		return nil, fmt.Errorf("store: list unrevoked run pats: %w", err)
	}
	defer rows.Close()
	out := []RunPAT{}
	for rows.Next() {
		var p RunPAT
		if err := rows.Scan(&p.RunID, &p.AuthorizationID, &p.Owner, &p.ProviderRowID, &p.Org, &p.Scope,
			&p.ValidTo, &p.CreatedAt, &p.RevokedAt, &p.RevokeReason, &p.LastError); err != nil {
			return nil, fmt.Errorf("store: scan run pat: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: list unrevoked run pats: %w", err)
	}
	return out, nil
}

// MemRunPATs is RunPATStore in memory, for the tests of every lane that
// creates or revokes a token and has no database.
type MemRunPATs struct {
	mu   sync.Mutex
	rows []RunPAT
}

// NewMemRunPATs returns an empty MemRunPATs.
func NewMemRunPATs() *MemRunPATs { return &MemRunPATs{} }

func (m *MemRunPATs) InsertRunPAT(_ context.Context, p RunPAT) error {
	if err := p.validate(); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, r := range m.rows {
		if r.RunID == p.RunID && r.AuthorizationID == p.AuthorizationID {
			return fmt.Errorf("%w: run token already recorded", ErrConflict)
		}
	}
	p.CreatedAt, p.RevokedAt, p.RevokeReason, p.LastError = time.Now().UTC(), nil, "", ""
	m.rows = append(m.rows, p)
	return nil
}

func (m *MemRunPATs) MarkRunPATRevoked(_ context.Context, runID, authorizationID uuid.UUID, reason, lastError string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.rows {
		r := &m.rows[i]
		if r.RunID == runID && r.AuthorizationID == authorizationID && r.RevokedAt == nil {
			at := time.Now().UTC()
			r.RevokedAt, r.RevokeReason, r.LastError = &at, reason, lastError
			return true, nil
		}
	}
	return false, nil
}

func (m *MemRunPATs) ListUnrevokedRunPATs(_ context.Context, f RunPATFilter) ([]RunPAT, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []RunPAT{}
	for _, r := range m.rows {
		if r.RevokedAt == nil && f.matches(r) {
			out = append(out, r)
		}
	}
	slices.SortStableFunc(out, func(a, b RunPAT) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return slices.Compare(a.AuthorizationID[:], b.AuthorizationID[:])
	})
	return out, nil
}
