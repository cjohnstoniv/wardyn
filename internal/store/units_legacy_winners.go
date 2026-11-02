// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MembershipClaims is one person's claims as ListMembershipsForQ matches them: user subjects in the
// order they rank, groups, and a user type ("" for none).
type MembershipClaims struct {
	Users    []string `json:"users"`
	Groups   []string `json:"groups"`
	UserType string   `json:"user_type"`
}

// LegacyWinnersQ is, for each of people, the first legacy row ListMembershipsForQ would list for
// their claims, in its ORDER BY, read in one statement however many people there are. The zero match
// for a person no legacy row places.
func LegacyWinnersQ(ctx context.Context, q Querier, people []MembershipClaims) ([]MembershipMatch, error) {
	out := make([]MembershipMatch, len(people))
	if len(people) == 0 {
		return out, nil
	}
	norm := make([]MembershipClaims, len(people))
	for i, c := range people { // a nil slice encodes as JSON null, which has no elements to read
		norm[i] = MembershipClaims{Users: append([]string{}, c.Users...), Groups: append([]string{}, c.Groups...), UserType: c.UserType}
	}
	claims, err := json.Marshal(norm)
	if err != nil {
		return nil, fmt.Errorf("store: encode legacy winner claims: %w", err)
	}
	stmt := `SELECT k.i, m.id, m.subject_type, m.subject, m.unit_id, m.priority, m.created_at, m.created_by, m.home, m.legacy, ` + governanceUnitColsP + `
		FROM (SELECT c.i,
				ARRAY(SELECT jsonb_array_elements_text(c.p->'users')) AS users,
				ARRAY(SELECT jsonb_array_elements_text(c.p->'groups')) AS groups,
				COALESCE(c.p->>'user_type', '') AS user_type
			FROM jsonb_array_elements($1::jsonb) WITH ORDINALITY AS c(p, i)) k
		JOIN LATERAL (SELECT m.id FROM governance_memberships m
			JOIN governance_units p ON p.id = m.unit_id
			WHERE m.legacy AND ` + subjectMatchOf("m", "k.users", "k.groups", "k.user_type") + `
			ORDER BY ` + subjectPrecedenceOf("m", "p", "k.users") + `
			LIMIT 1) w ON true
		JOIN governance_memberships m ON m.id = w.id
		JOIN governance_units p ON p.id = m.unit_id`
	rows, err := q.Query(ctx, stmt, claims)
	if err != nil {
		return nil, fmt.Errorf("store: read legacy winners: %w", err)
	}
	_, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (struct{}, error) {
		var i int
		var m MembershipMatch
		a := &m.Membership
		u, err := scanGovernanceUnitPrefixed(r, &i, &a.ID, &a.SubjectType, &a.Subject, &a.UnitID, &a.Priority, &a.CreatedAt, &a.CreatedBy, &a.Home, &a.Legacy)
		if err == nil && (i < 1 || i > len(out)) {
			err = fmt.Errorf("store: legacy winner for person %d of %d", i, len(out))
		}
		if err == nil {
			m.Unit = u
			out[i-1] = m
		}
		return struct{}{}, err
	})
	if err != nil {
		return nil, fmt.Errorf("store: iterate legacy winners: %w", err)
	}
	return out, nil
}

// LegacyRenameRivalsQ pairs each legacy row l outside unit with one legacy row d of unit it ties with
// up to the unit name (one subject type and priority, and outside the group tier one subject), for
// every l that has one. d is, of the tied rows of unit that no row of l's subject in unit ranks before,
// the one whose subject's first row outside unit ranks last (none ranks last of all); with no such
// row, unit's first tied row. Ranks are the order ListMembershipsForQ gives a person of groups alone
// or of one subject, in which a rename of unit reorders no two rows both in unit or both outside it.
// So when any tied d ranks first in unit and l first outside it for the person d and l alone place,
// this d does too, and a rename trades that person's winner between unit and l's unit as it trades
// any person's whose first rows there are d and l. One statement that sorts the legacy rows: at most
// one pair for each row outside unit, never every tied pair.
func LegacyRenameRivalsQ(ctx context.Context, q Querier, unit uuid.UUID) ([][2]types.GovernanceMembership, error) {
	stmt := `WITH r AS (
			SELECT m.id, m.subject_type, m.subject, m.priority, m.unit_id = $1 AS own,
				CASE m.subject_type WHEN 'group' THEN '' ELSE m.subject END AS tie,
				row_number() OVER (ORDER BY ` + subjectPrecedenceOf("m", "p", "'{}'::text[]") + `) AS rk
			FROM governance_memberships m JOIN governance_units p ON p.id = m.unit_id
			WHERE m.legacy),
		s AS (
			SELECT subject_type, subject, min(rk) FILTER (WHERE own) AS own_rk, min(rk) FILTER (WHERE NOT own) AS other_rk
			FROM r GROUP BY subject_type, subject),
		w AS (
			SELECT r.id, r.own,
				max(ARRAY[COALESCE(s.other_rk, 9223372036854775807), r.rk]) FILTER (WHERE r.own) OVER (
					PARTITION BY r.subject_type, r.priority, r.tie
					ORDER BY CASE WHEN r.own THEN r.rk ELSE s.own_rk END, r.own DESC
					ROWS UNBOUNDED PRECEDING) AS reach,
				min(r.rk) FILTER (WHERE r.own) OVER (PARTITION BY r.subject_type, r.priority, r.tie) AS first_rk
			FROM r JOIN s ON s.subject_type = r.subject_type AND s.subject = r.subject)
		SELECT d.id, d.subject_type, d.subject, d.unit_id, d.priority, d.created_at, d.created_by, d.home, d.legacy,
			l.id, l.subject_type, l.subject, l.unit_id, l.priority, l.created_at, l.created_by, l.home, l.legacy
		FROM w JOIN r rd ON rd.rk = COALESCE(w.reach[2], w.first_rk)
		JOIN governance_memberships d ON d.id = rd.id
		JOIN governance_memberships l ON l.id = w.id
		WHERE NOT w.own`
	return collect(ctx, q, "read", "legacy rename rivals", stmt, []any{unit}, func(row pgx.Row) ([2]types.GovernanceMembership, error) {
		var p [2]types.GovernanceMembership
		var dst []any
		for i := range p {
			a := &p[i]
			dst = append(dst, &a.ID, &a.SubjectType, &a.Subject, &a.UnitID, &a.Priority, &a.CreatedAt, &a.CreatedBy, &a.Home, &a.Legacy)
		}
		return p, row.Scan(dst...)
	})
}
