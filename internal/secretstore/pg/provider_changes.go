// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package pg

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/db"
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
// provider purge can write or delete history for, the way a fenced Put does
// (checkGeneration), until the purge's transaction ends, and returns them. An
// erase's advance then waits for the purge to commit, and its
// DeleteProviderChanges removes what the purge wrote.
//
// It covers every invalidation of the call at once, a provider removal
// included, and runs before any history row is written or deleted. The owners
// are locked in one pass in name order, so the erase, which takes an owner's
// counter row and then that owner's history, meets the purge's locks in the
// same order.
//
// The counter rows are held shared, so they do not order two purges against
// each other: recordProviderChanges takes db.ProviderPurgeLockClass first, and
// two purges never overlap. Both callers of a purge (PUT /site-config and the
// model-provider block write) also hold the site-config door lock, but that
// lock is a session lock held on another connection, so the store does not
// rely on it.
//
// The owners are chosen by one statement and the history is written by later
// ones, so a credential that commits between them belongs to an owner who is
// not locked. recordProviderChanges therefore writes only for the returned
// owners: that owner gets no history row from this purge, which is a lost
// notice and not a row an erase can miss.
func lockProviderOwners(ctx context.Context, tx pgx.Tx, invalidations []secretstore.ProviderInvalidation) ([]string, error) {
	var names, providers []string
	for _, inv := range invalidations {
		providers = append(providers, inv.Change.ProviderID)
		if inv.Change.ProviderUID != "" {
			names = append(names, providerSecretNames(inv.OldUID)...)
		}
	}
	rows, err := tx.Query(ctx, `SELECT owner FROM (
     SELECT owned_by AS owner FROM secrets WHERE name=ANY($1) AND owned_by<>''
     UNION SELECT owner FROM provider_connection_changes WHERE provider_id=ANY($2)
    ) affected ORDER BY owner`, names, providers)
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: record provider change: %w", err)
	}
	owners, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, fmt.Errorf("pg secretstore: record provider change: %w", err)
	}
	for _, owner := range owners {
		if _, err := tx.Exec(ctx, `INSERT INTO secret_owner_generations (owned_by) VALUES ($1) ON CONFLICT (owned_by) DO NOTHING`, owner); err != nil {
			return nil, fmt.Errorf("pg secretstore: record provider change: %w", err)
		}
		if _, err := tx.Exec(ctx, `SELECT 1 FROM secret_owner_generations WHERE owned_by=$1 FOR SHARE`, owner); err != nil {
			return nil, fmt.Errorf("pg secretstore: record provider change: %w", err)
		}
	}
	return owners, nil
}

// providerSecretNames are the credential names a provider uid's connections are stored under.
func providerSecretNames(uid string) []string {
	names := make([]string, 0, 4)
	for _, part := range []string{"key", "oauth", "sso", "entra"} {
		names = append(names, types.ModelProviderSecretPrefix+uid+"-"+part)
	}
	return names
}

func recordProviderChanges(ctx context.Context, tx pgx.Tx) error {
	invalidations := secretstore.ProviderInvalidationsFrom(ctx)
	if len(invalidations) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1, 0)`, db.ProviderPurgeLockClass); err != nil {
		return fmt.Errorf("pg secretstore: record provider change: %w", err)
	}
	locked, err := lockProviderOwners(ctx, tx, invalidations)
	if err != nil {
		return err
	}
	for _, invalidation := range invalidations {
		c := invalidation.Change
		if c.ProviderUID != "" {
			// Existing history follows an edit, but never a deletion and recreation.
			_, err := tx.Exec(ctx, `INSERT INTO provider_connection_changes
    (owner, provider_id, provider_uid, reason, changed_at, new_destination)
    SELECT owner, $2, $3, $4, $5, $6 FROM (
     SELECT owned_by AS owner FROM secrets WHERE name=ANY($1) AND owned_by<>''
     UNION SELECT owner FROM provider_connection_changes WHERE provider_id=$2
    ) affected WHERE owner = ANY($7)
    ON CONFLICT (owner, provider_uid) DO UPDATE SET
     provider_id=EXCLUDED.provider_id, reason=EXCLUDED.reason,
     changed_at=EXCLUDED.changed_at, new_destination=EXCLUDED.new_destination`,
				providerSecretNames(invalidation.OldUID), c.ProviderID, c.ProviderUID, c.Reason, c.ChangedAt, c.NewDestination, locked)
			if err != nil {
				return fmt.Errorf("pg secretstore: record provider change: %w", err)
			}
		}
		if _, err := tx.Exec(ctx, `DELETE FROM provider_connection_changes WHERE provider_id=$1 AND provider_uid<>$2 AND owner = ANY($3)`, c.ProviderID, c.ProviderUID, locked); err != nil {
			return fmt.Errorf("pg secretstore: purge provider history: %w", err)
		}
	}
	return nil
}
