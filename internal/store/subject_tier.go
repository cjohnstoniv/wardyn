// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
)

// The SQL half of the one precedence-select shared by governance assignments and user
// drive grants; internal/api's selectByTier is the Go half. Keep both in sync — UT-3
// broke when a new tier was added to only one side.

// subjectTierOrder ranks tiers most-specific-first: user > group > user_type > all.
// subject_type stays unqualified so it works in both JOINs and single-table listings;
// safe only while no joined table has its own subject_type column, else Postgres errors
// on the ambiguity instead of silently mis-ranking.
const subjectTierOrder = `CASE subject_type WHEN 'user' THEN 0 WHEN 'group' THEN 1 WHEN 'user_type' THEN 2 ELSE 3 END`

// subjectMatch builds the WHERE clause matching subject rows aliased row: $1 user
// subjects, $2 groups, $3 the caller's single user type ("" matches nothing).
func subjectMatch(row string) string {
	return fmt.Sprintf(`(%[1]s.subject_type = 'all'
		   OR (%[1]s.subject_type = 'user'  AND %[1]s.subject = ANY($1::text[]))
		   OR (%[1]s.subject_type = 'group' AND %[1]s.subject = ANY($2::text[]))
		   OR (%[1]s.subject_type = 'user_type' AND %[1]s.subject = $3))`, row)
}

// subjectPrecedence is the ORDER BY ranking subject rows (row) joined to named rows
// (named):
//  1. tier (subjectTierOrder) — an explicit single-principal row always beats a group row.
//  2. within the user tier, sub-keyed match beats email: sub is stable, so a departed
//     colleague's row can't be inherited by reusing their email.
//  3. priority DESC, then name ASC (UNIQUE, for a deterministic LIMIT 1).
//  4. subject ASC as a total-order floor — two rows can target the same profile/drive
//     with different overrides, so a tie could otherwise silently drop an admin's narrowing.
func subjectPrecedence(row, named string) string {
	return subjectTierOrder + fmt.Sprintf(`,
			CASE %[1]s.subject_type WHEN 'user'
				THEN COALESCE(array_position($1::text[], %[1]s.subject), 2147483647)
				ELSE 0 END,
			%[1]s.priority DESC,
			%[2]s.name ASC,
			%[1]s.subject ASC`, row, named)
}

// hasGroupTierRows gates the stale/truncated-snapshot refusal: it must fire only when
// group rows actually exist to evaluate, or every pre-upgrade/no-group deployment would
// wrongly refuse. table must be one of this package's literals, never caller input.
func (s PG) hasGroupTierRows(ctx context.Context, table, what string) (bool, error) {
	var has bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE subject_type = 'group')`).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: check group-tier %s: %w", what, err)
	}
	return has, nil
}
