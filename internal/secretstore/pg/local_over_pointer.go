// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
)

// putOverPointer is a local Put on a store that still reads an external store's pointer rows. Under
// the row lock it notes the object the row points to, writes the local row and commits; the old
// object, which no row names any more, is then removed with removeOldExternal's recheck. The Put has
// succeeded by then, so a failed removal is logged, as putExternal's removeOld logs one.
func (s *Store) putOverPointer(ctx context.Context, name string, r sealedRow) error {
	ref := rowRef(s.owner, name)
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: put %s: begin: %w", ref, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
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
	if _, err := s.removeOldExternal(context.WithoutCancel(ctx), s.owner, name, store, loc); err != nil {
		slog.Warn("pg secretstore: a replaced value's old object was not removed; `wardynd -reconcile` lists it",
			slog.String("secret", ref), slog.String("store", store), slog.Any("err", err))
	}
	return nil
}
