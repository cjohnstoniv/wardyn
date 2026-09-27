// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
)

var _ secretstore.MetaStore = (*Store)(nil)

// MarkUsed stamps last_used_at on this view's own row of name — see
// secretstore.MetaStore. The minute is the design's resolution (§2.2): "last
// used" is read at human granularity, so a person whose runs resolve their
// key many times a minute costs one row write a minute, and every resolve
// inside that minute is a primary-key lookup that writes nothing. The window
// is in SQL, so it holds across replicas without shared state.
func (s *Store) MarkUsed(ctx context.Context, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE secrets SET last_used_at = now()
		WHERE owned_by=$1 AND name=$2 AND (last_used_at IS NULL OR last_used_at < now() - interval '1 minute')`,
		s.owner, name)
	if err != nil {
		return fmt.Errorf("pg secretstore: mark %s used: %w", rowRef(s.owner, name), err)
	}
	return nil
}

// Metadata returns this view's own rows of names; see secretstore.MetaStore.
func (s *Store) Metadata(ctx context.Context, names []string) ([]secretstore.Meta, error) {
	return s.metadata(ctx, names, false)
}

// MetadataEverywhere returns every person's rows of names; see
// secretstore.MetaStore.
func (s *Store) MetadataEverywhere(ctx context.Context, names []string) ([]secretstore.Meta, error) {
	return s.metadata(ctx, names, true)
}

// metadata reads the rows' metadata columns only: the value, its data key and
// its ref are never selected. everyone widens the view's own rows to every
// person's.
func (s *Store) metadata(ctx context.Context, names []string, everyone bool) ([]secretstore.Meta, error) {
	rows, err := s.pool.Query(ctx, `SELECT owned_by, name, enc_version, split_part(kek_id, ':', 1), created_at, last_used_at, expires_at
		FROM secrets WHERE name = ANY($1) AND CASE WHEN $3 THEN owned_by <> '' ELSE owned_by = $2 END ORDER BY owned_by, name`,
		names, s.owner, everyone)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: metadata: %w: %w", secretstore.ErrUnavailable, err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (secretstore.Meta, error) {
		var m secretstore.Meta
		var version int16
		var store string
		err := r.Scan(&m.Owner, &m.Name, &version, &store, &m.AddedAt, &m.LastUsedAt, &m.ExpiresAt)
		m.Store = "pg"
		if version == extVersion {
			m.Store = store
		}
		return m, err
	})
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: metadata scan: %w", err)
	}
	return out, nil
}
