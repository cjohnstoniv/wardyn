// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/jackc/pgx/v5"
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
