// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
)

// The SQL half of the one precedence-select (authz design K3). Governance
// assignments and user drive grants pick ONE winning row per caller over the
// same subject vocabulary, so the match and the ranking are written once here
// and spliced into both resolvers and both console listings. Two copies of one
// rule is how one of them quietly stops ranking a new tier: UT-3 had to insert
// user_type into each by hand. internal/api's selectByTier is the Go half, the
// one stale-snapshot rule over what these resolvers return.

// subjectTierOrder ranks the four subject tiers MOST SPECIFIC FIRST —
// user > group > user_type > all. A person's type sits below their groups so a
// group row overrides the type's, and above `all` so a type's row binds
// everyone of that type.
//
// subject_type is deliberately UNQUALIFIED so the one string works in the
// resolvers' JOINs as well as the single-table listings. That is safe because
// neither joined table (governance_profiles, user_drives) has a subject_type
// column. A migration that added one would make this ambiguous, and Postgres
// would say so loudly rather than silently re-rank.
const subjectTierOrder = `CASE subject_type WHEN 'user' THEN 0 WHEN 'group' THEN 1 WHEN 'user_type' THEN 2 ELSE 3 END`

// subjectMatch is the WHERE a resolver filters the subject rows aliased row
// with. $1 is the caller's user subjects, $2 their groups, $3 their one user
// type ("" matches no row).
func subjectMatch(row string) string {
	return fmt.Sprintf(`(%[1]s.subject_type = 'all'
		   OR (%[1]s.subject_type = 'user'  AND %[1]s.subject = ANY($1::text[]))
		   OR (%[1]s.subject_type = 'group' AND %[1]s.subject = ANY($2::text[]))
		   OR (%[1]s.subject_type = 'user_type' AND %[1]s.subject = $3))`, row)
}

// subjectPrecedence is the ORDER BY both resolvers rank by, over the subject
// rows aliased row joined to the named rows aliased named. Ranked, in order:
//
//  1. tier (subjectTierOrder). A row is one admin explicitly naming one
//     principal, so the more specific naming wins outright; no priority in the
//     group tier can beat a user-tier row. A person holds one type, so the type
//     tier matches at most one row.
//  2. within the user tier, a sub-keyed match beats an email-keyed one.
//     capabilitySubjects returns up to TWO user subjects (lowercased sub, then
//     email) and an admin may have written a row against either. Sub wins
//     because it is the stable identifier: an email is reassignable, and
//     inheriting a departed colleague's ceiling or drive by taking their
//     address is not a thing this may permit. Encoded as the MATCH POSITION in
//     the caller's own $1 (array_position), so the caller's documented ordering
//     IS the precedence.
//  3. priority DESC: the admin's explicit tie-break, and the group tier's
//     working lever (a person is usually in several groups at once).
//  4. named.name ASC, in EVERY tier: without it two same-priority rows make
//     LIMIT 1 depend on the plan. Both named tables have a UNIQUE name.
//  5. row.subject ASC, the total-order FLOOR. Two rows can name the SAME
//     profile or drive (UNIQUE(subject_type, subject) is per subject), and a
//     drive grant carries per-row overrides (writable, size, home, enabled), so
//     the losing coin-flip would be an admin's read-only narrowing silently not
//     applying. Subject is distinct within a tier and explainable ("the
//     alphabetically first group's row wins"), which a row id would not be.
func subjectPrecedence(row, named string) string {
	return subjectTierOrder + fmt.Sprintf(`,
			CASE %[1]s.subject_type WHEN 'user'
				THEN COALESCE(array_position($1::text[], %[1]s.subject), 2147483647)
				ELSE 0 END,
			%[1]s.priority DESC,
			%[2]s.name ASC,
			%[1]s.subject ASC`, row, named)
}

// hasGroupTierRows reports whether ANY group-tier row exists in table.
//
// It is the gate on the stale/truncated-snapshot refusal, and it is a separate,
// deliberately cheap read because that refusal must fire on exactly one
// deployment shape. A caller whose group snapshot is missing or truncated
// cannot have their group rows evaluated, but on a deployment with NO
// group-tier rows there is nothing an unknown group could have matched, so
// refusing there would break "no row ⇒ exactly as before" for every
// pre-upgrade session and every deployment that never adopted group rows.
// EXISTS, not a count: the answer is a boolean and Postgres stops at the first
// row. table is one of this package's literals, never caller input.
func (s PG) hasGroupTierRows(ctx context.Context, table, what string) (bool, error) {
	var has bool
	err := s.Pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM `+table+` WHERE subject_type = 'group')`).Scan(&has)
	if err != nil {
		return false, fmt.Errorf("store: check group-tier %s: %w", what, err)
	}
	return has, nil
}
