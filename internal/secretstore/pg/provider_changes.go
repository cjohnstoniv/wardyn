// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ProviderChange returns only this owner's change record for the current UID.
func (s *Store) ProviderChange(ctx context.Context, uid string) (secretstore.ProviderChange, bool, error) {
	var c secretstore.ProviderChange
	err := s.pool.QueryRow(ctx, `SELECT owner, provider_id, provider_uid, reason, changed_at, new_destination
 FROM provider_connection_changes WHERE owner=$1 AND provider_uid=$2`, s.owner, uid).
		Scan(&c.Owner, &c.ProviderID, &c.ProviderUID, &c.Reason, &c.ChangedAt, &c.NewDestination)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, false, nil
	}
	if err != nil {
		return c, false, fmt.Errorf("pg secretstore: provider change: %w", err)
	}
	return c, true, nil
}

// DeleteProviderChanges removes all connection history for this owner. It takes
// the owner's counter row exclusively first, which waits for a provider purge
// that holds the row shared (lockProviderOwners) and makes a later one read the
// history after this delete, so no history row is written back behind it.
func (s *Store) DeleteProviderChanges(ctx context.Context) error {
	tx, err := beginReadCommitted(ctx, s.pool)
	if err != nil {
		return fmt.Errorf("pg secretstore: erase provider history: begin: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if s.owner != "" {
		if _, err := tx.Exec(ctx, `INSERT INTO secret_owner_generations (owned_by) VALUES ($1) ON CONFLICT (owned_by) DO NOTHING`, s.owner); err != nil {
			return fmt.Errorf("pg secretstore: erase provider history: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM secret_owner_generations WHERE owned_by=$1 FOR UPDATE`, s.owner); err != nil {
			return fmt.Errorf("pg secretstore: erase provider history: %w", err)
		}
	}
	if _, err := tx.Exec(ctx, `DELETE FROM provider_connection_changes WHERE owner=$1`, s.owner); err != nil {
		return fmt.Errorf("pg secretstore: erase provider history: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("pg secretstore: erase provider history: commit: %w", err)
	}
	return nil
}

// lockProviderOwners share-locks the erase-generation row of every owner a
// provider purge is about to write history for, the way a fenced Put does
// (checkGeneration), until the purge's transaction ends. An erase's advance
// then waits for the purge to commit, and its DeleteProviderChanges removes
// what the purge wrote. Owners are locked in name order, so two purges cannot
// deadlock; the history is written by a later statement, which therefore sees
// every erase that committed before the locks were granted.
func lockProviderOwners(ctx context.Context, tx pgx.Tx, names []string, providerID string) error {
	rows, err := tx.Query(ctx, `SELECT owner FROM (
     SELECT owned_by AS owner FROM secrets WHERE name=ANY($1) AND owned_by<>''
     UNION SELECT owner FROM provider_connection_changes WHERE provider_id=$2
    ) affected ORDER BY owner`, names, providerID)
	if err != nil {
		return fmt.Errorf("pg secretstore: record provider change: %w", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return fmt.Errorf("pg secretstore: record provider change: %w", err)
	}
	for _, owner := range owners {
		if _, err := tx.Exec(ctx, `INSERT INTO secret_owner_generations (owned_by) VALUES ($1) ON CONFLICT (owned_by) DO NOTHING`, owner); err != nil {
			return fmt.Errorf("pg secretstore: record provider change: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM secret_owner_generations WHERE owned_by=$1 FOR SHARE`, owner); err != nil {
			return fmt.Errorf("pg secretstore: record provider change: %w", err)
		}
	}
	return nil
}

func recordProviderChanges(ctx context.Context, tx pgx.Tx) error {
	for _, invalidation := range secretstore.ProviderInvalidationsFrom(ctx) {
		c := invalidation.Change
		if c.ProviderUID != "" {
			names := make([]string, 0, 4)
			for _, part := range []string{"key", "oauth", "sso", "entra"} {
				names = append(names, types.ModelProviderSecretPrefix+invalidation.OldUID+"-"+part)
			}
			if err := lockProviderOwners(ctx, tx, names, c.ProviderID); err != nil {
				return err
			}
			// Existing history follows an edit, but never a deletion and recreation.
			_, err := tx.Exec(ctx, `INSERT INTO provider_connection_changes
    (owner, provider_id, provider_uid, reason, changed_at, new_destination)
    SELECT owner, $2, $3, $4, $5, $6 FROM (
     SELECT owned_by AS owner FROM secrets WHERE name=ANY($1) AND owned_by<>''
     UNION SELECT owner FROM provider_connection_changes WHERE provider_id=$2
    ) affected
    ON CONFLICT (owner, provider_uid) DO UPDATE SET
     provider_id=EXCLUDED.provider_id, reason=EXCLUDED.reason,
     changed_at=EXCLUDED.changed_at, new_destination=EXCLUDED.new_destination`,
				names, c.ProviderID, c.ProviderUID, c.Reason, c.ChangedAt, c.NewDestination)
			if err != nil {
				return fmt.Errorf("pg secretstore: record provider change: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM provider_connection_changes WHERE provider_id=$1 AND provider_uid<>$2`, c.ProviderID, c.ProviderUID); err != nil {
			return fmt.Errorf("pg secretstore: purge provider history: %w", err)
		}
	}
	return nil
}
