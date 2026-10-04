// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The Azure DevOps sign-in end counter (migration 0121): per person, how many times their
// sign-in was disconnected or erased and whether one is running now, so a run token created
// on any replica across one of those ends revokes itself. The row holds no secret.
package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ADOSignInEndsStaleAfter is how long an end that began and never finished (its replica died)
// still counts as running. Past it the count settles, so a person's mints stop revoking
// themselves; an end finishes in seconds.
const ADOSignInEndsStaleAfter = 10 * time.Minute

// ADOSignInEndState is one person's count.
type ADOSignInEndState struct {
	Gen     int64
	Running bool // an end began and has not finished, within ADOSignInEndsStaleAfter
	Reason  string
}

// ADOSignInEndStore is the sign-in end counter. Optional like RunOutputStore: the api layer
// type-asserts, and a store without it counts in the process.
type ADOSignInEndStore interface {
	// BeginADOSignInEnd marks the start of an end of owner's sign-in.
	BeginADOSignInEnd(ctx context.Context, owner, reason string) error
	// FinishADOSignInEnd marks its finish.
	FinishADOSignInEnd(ctx context.Context, owner string) error
	// ADOSignInEnds is owner's count now; a person with no row has gen 0.
	ADOSignInEnds(ctx context.Context, owner string) (ADOSignInEndState, error)
}

var _ ADOSignInEndStore = PG{}

// BeginADOSignInEnd — see ADOSignInEndStore.
func (s PG) BeginADOSignInEnd(ctx context.Context, owner, reason string) error {
	if _, err := s.Pool.Exec(ctx, `
		INSERT INTO ado_signin_ends (owner, gen, active, reason) VALUES ($1, 1, 1, $2)
		ON CONFLICT (owner) DO UPDATE SET gen = ado_signin_ends.gen + 1,
			active = CASE WHEN ado_signin_ends.updated_at < now() - $3::interval THEN 1 ELSE ado_signin_ends.active + 1 END,
			reason = EXCLUDED.reason, updated_at = now()`,
		owner, reason, ADOSignInEndsStaleAfter.String()); err != nil {
		return fmt.Errorf("store: begin a sign-in end: %w", err)
	}
	return nil
}

// FinishADOSignInEnd — see ADOSignInEndStore.
func (s PG) FinishADOSignInEnd(ctx context.Context, owner string) error {
	if _, err := s.Pool.Exec(ctx, `
		UPDATE ado_signin_ends SET gen = gen + 1, active = GREATEST(active - 1, 0), updated_at = now()
		WHERE owner = $1`, owner); err != nil {
		return fmt.Errorf("store: finish a sign-in end: %w", err)
	}
	return nil
}

// ADOSignInEnds — see ADOSignInEndStore.
func (s PG) ADOSignInEnds(ctx context.Context, owner string) (ADOSignInEndState, error) {
	var st ADOSignInEndState
	var active int
	var stale bool
	err := s.Pool.QueryRow(ctx, `
		SELECT gen, active, reason, updated_at < now() - $2::interval FROM ado_signin_ends WHERE owner = $1`,
		owner, ADOSignInEndsStaleAfter.String()).Scan(&st.Gen, &active, &st.Reason, &stale)
	if errors.Is(err, pgx.ErrNoRows) {
		return ADOSignInEndState{}, nil
	}
	if err != nil {
		return ADOSignInEndState{}, fmt.Errorf("store: read a sign-in end count: %w", err)
	}
	st.Running = active > 0 && !stale
	return st, nil
}
