// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

var _ secretstore.Generational = (*Store)(nil)

// Generation is this view's namespace's erase generation: 0 for one never
// erased, which is also every namespace that predates the counter. The operator
// namespace is never erased, so it is always 0.
func (s *Store) Generation(ctx context.Context) (int64, error) {
	if s.owner == "" {
		return 0, nil
	}
	var gen int64
	err := s.pool.QueryRow(ctx, `SELECT COALESCE((SELECT generation FROM secret_owner_generations WHERE owned_by=$1), 0)`, s.owner).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: erase generation of %q: %w: %w", s.owner, secretstore.ErrUnavailable, err)
	}
	return gen, nil
}

// AdvanceGeneration is EraseOwner's first step. The upsert takes the counter
// row's exclusive lock, which waits for every fenced Put that holds it shared
// (checkGeneration), so a write that passed the old value has committed before
// this returns and a write checking afterwards sees the new one.
func (s *Store) AdvanceGeneration(ctx context.Context) (int64, error) {
	if s.owner == "" {
		return 0, secretstore.ErrOperatorNamespace
	}
	var gen int64
	err := s.pool.QueryRow(ctx, `
		INSERT INTO secret_owner_generations (owned_by, generation) VALUES ($1, 1)
		ON CONFLICT (owned_by) DO UPDATE SET generation = secret_owner_generations.generation + 1, updated_at = now()
		RETURNING generation`, s.owner).Scan(&gen)
	if err != nil {
		return 0, fmt.Errorf("pg secretstore: advance erase generation of %q: %w", s.owner, err)
	}
	return gen, nil
}

// checkGeneration refuses a fenced write whose owner has been erased since it
// began. It runs inside the write's transaction and keeps the counter row
// share-locked until that transaction ends, which is what orders the write
// against AdvanceGeneration: either the write commits first and the erase
// deletes it, or the erase commits first and the write is refused here. An
// owner with no counter row is created at 0 so the lock has a row to take.
func (s *Store) checkGeneration(ctx context.Context, tx pgx.Tx, want int64) error {
	var cur int64
	const lock = `SELECT generation FROM secret_owner_generations WHERE owned_by=$1 FOR SHARE`
	err := tx.QueryRow(ctx, lock, s.owner).Scan(&cur)
	if errors.Is(err, pgx.ErrNoRows) {
		if _, err = tx.Exec(ctx, `INSERT INTO secret_owner_generations (owned_by) VALUES ($1) ON CONFLICT (owned_by) DO NOTHING`, s.owner); err == nil {
			err = tx.QueryRow(ctx, lock, s.owner).Scan(&cur)
		}
	}
	if err != nil {
		return fmt.Errorf("pg secretstore: put for %q: read erase generation: %w", s.owner, err)
	}
	if cur != want {
		return fmt.Errorf("pg secretstore: put for %q: %w", s.owner, secretstore.ErrGenerationChanged)
	}
	return nil
}

// generationFence is the generation a Put under ctx is fenced to, when it is
// fenced at all: only a person's namespace has one.
func (s *Store) generationFence(ctx context.Context) (int64, bool) {
	if s.owner == "" {
		return 0, false
	}
	return secretstore.IfGenerationFrom(ctx, s.owner)
}

// putFenced is the local-mode Put under an erase generation: the check and the
// write share one transaction, so the counter row's share lock is held until
// the row is committed.
func (s *Store) putFenced(ctx context.Context, name string, gen int64, r sealedRow) error {
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: begin: %w", rowRef(s.owner, name), err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := s.checkGeneration(ctx, tx, gen); err != nil {
		return err
	}
	if err := s.putRow(ctx, tx, name, r); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg secretstore: put %s: commit: %w", rowRef(s.owner, name), err)
	}
	return nil
}
