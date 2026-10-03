// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package subjectkey

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/secretstore/kek"
)

// Rewrap moves, inside tx, every live generation's wrap from the KEK source
// names for it onto the one target names, and returns how many it moved. It is
// root rotation's half of `wardynd -rewrap` and `-rotate-age-key`: the caller
// runs it in the transaction that moves the `secrets` rows, so both commit or
// neither does, and a key version is reported retirable only when both tables
// are at it.
//
// target returns nil for a row the operation leaves alone. A row already under
// its target, and at latest[its kek_id] when that is set, is skipped. rotated,
// when not nil, collects the kek_id of each versioned target that wrapped a row
// under a version other than latest's. A generation is moved within its own
// domain, never across domains, and its key is unwrapped and wrapped again
// under the same (owner, purpose, version, domain) binding; the key is never
// kept past the row.
func Rewrap(ctx context.Context, tx pgx.Tx, source func(domain, kekID string) (kek.KEK, error), target func(domain, kekID string) (kek.KEK, error), latest map[string]string, rotated map[string]bool) (int, error) {
	type row struct {
		owner, purpose, domain, kekID string
		version                       int
		wrapped                       []byte
	}
	rows, err := tx.Query(ctx, `SELECT owner, purpose, version, domain, kek_id, wrapped_key FROM principal_keys
		WHERE destroyed_at IS NULL ORDER BY owner, purpose, version FOR UPDATE`)
	if err != nil {
		return 0, fmt.Errorf("principal_keys select: %w", err)
	}
	all, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var e row
		err := r.Scan(&e.owner, &e.purpose, &e.version, &e.domain, &e.kekID, &e.wrapped)
		return e, err
	})
	if err != nil {
		return 0, fmt.Errorf("principal_keys scan: %w", err)
	}
	n := 0
	for _, e := range all {
		ref := fmt.Sprintf("principal key (owner=%q, purpose=%q, version=%d, domain=%q)", e.owner, e.purpose, e.version, e.domain)
		to, err := target(e.domain, e.kekID)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", ref, err)
		}
		if to == nil {
			continue
		}
		if e.kekID == to.ID() {
			old, err := kek.Behind(to, e.wrapped, latest[to.ID()])
			if err != nil {
				return 0, fmt.Errorf("%s: %w", ref, err)
			}
			if !old {
				continue
			}
		}
		from, err := source(e.domain, e.kekID)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", ref, err)
		}
		bind := kek.PrincipalBind(e.owner, e.purpose, e.version, e.domain)
		key, err := from.Unwrap(ctx, e.wrapped, bind)
		if err != nil {
			return 0, fmt.Errorf("%s: unwrap with the old key: %w", ref, err)
		}
		wrapped, err := to.Wrap(ctx, key, bind)
		clear(key)
		if err != nil {
			return 0, fmt.Errorf("%s: wrap with the new key: %w", ref, err)
		}
		if rotated != nil {
			was, err := kek.Behind(to, wrapped, latest[to.ID()])
			if err != nil {
				return 0, fmt.Errorf("%s: %w", ref, err)
			}
			if was {
				rotated[to.ID()] = true
			}
		}
		if _, err := tx.Exec(ctx,
			`UPDATE principal_keys SET kek_id=$4, wrapped_key=$5 WHERE owner=$1 AND purpose=$2 AND version=$3`,
			e.owner, e.purpose, e.version, to.ID(), wrapped); err != nil {
			return 0, fmt.Errorf("%s: update: %w", ref, err)
		}
		n++
	}
	return n, nil
}
