// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
)

// putOverPointer is a local Put on a store that still reads an external store's pointer rows. Under
// the row lock it notes the object the row points to, writes the local row and commits; the old
// object, which no row names any more, is then removed after the Put returns (afterPut), with
// removeOld's recheck, which logs a failed removal.
func (s *Store) putOverPointer(ctx context.Context, name string, r sealedRow) error {
	ref := rowRef(s.owner, name)
	done, err := startPut(s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", ref, err)
	}
	defer done()
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: begin: %w", ref, err)
	}
	// Put sets no bound of its own, so the rollback takes db.Rollback's: ctx's deadline or db.RollbackBound.
	defer db.Rollback(ctx, tx)
	// The counter row's share lock before the row's advisory lock, putExternal's order.
	if gen, fenced := s.generationFence(ctx); fenced {
		if err := s.checkGeneration(ctx, tx, gen); err != nil {
			return err
		}
	}
	if err := lockRow(ctx, tx, s.owner, name); err != nil {
		return fmt.Errorf("pg secretstore: put %s: %w", ref, err)
	}
	var kekID string
	err = tx.QueryRow(ctx, `SELECT kek_id FROM secrets WHERE owned_by=$1 AND name=$2 AND enc_version=$3`,
		s.owner, name, extVersion).Scan(&kekID)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("pg secretstore: put %s: %w", ref, err)
	}
	if err := s.putRow(ctx, tx, name, r); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg secretstore: put %s: commit: %w", ref, err)
	}
	if kekID == "" {
		return nil
	}
	store, loc := splitRef(kekID)
	if !s.reachable(store) {
		slog.Warn("pg secretstore: a replaced value's old object is in a store this wardynd does not reach and was not removed",
			slog.String("secret", ref), slog.String("store", store))
		return nil
	}
	s.afterPut(ctx, name, func(ctx context.Context) { s.removeOld(ctx, name, loc) })
	return nil
}
