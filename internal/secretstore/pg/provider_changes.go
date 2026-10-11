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

// DeleteProviderChanges removes all connection history for this owner.
func (s *Store) DeleteProviderChanges(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, `DELETE FROM provider_connection_changes WHERE owner=$1`, s.owner); err != nil {
		return fmt.Errorf("pg secretstore: erase provider history: %w", err)
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
