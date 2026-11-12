// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"strings"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// unitAdminEmailsSQL walks parent_id up from the run's unit (bounded as the unit-scope read is) and
// reads the user-subject grants on any unit of that path. A subject is a sub or an email; a sub or an
// email matching a people row yields that person's address, and an email subject with no people row is
// its own address. A subject that is neither yields nothing.
const unitAdminEmailsSQL = `
WITH RECURSIVE chain AS (
	SELECT u.id, u.parent_id, 0 AS n FROM governance_units u WHERE u.id = $1
	UNION ALL
	SELECT u.id, u.parent_id, c.n + 1
	FROM governance_units u JOIN chain c ON u.id = c.parent_id
	WHERE c.n < 63
)
SELECT DISTINCT lower(btrim(s.addr)) FROM (
	SELECT COALESCE(NULLIF(p.email, ''), CASE WHEN a.subject LIKE '%@%' THEN a.subject END) AS addr
	  FROM chain c
	  JOIN governance_unit_admins a ON a.unit_id = c.id AND a.subject_type = 'user'
	  LEFT JOIN people p ON p.principal = a.subject OR lower(p.email) = lower(a.subject)
) s
 WHERE btrim(s.addr) <> ''
 ORDER BY 1`

// unitAdminEmails is the addresses of the people holding a user-subject unit-admin grant on unit or a
// unit above it. Only user-subject grants resolve: a group-subject grant is never expanded into a
// directory lookup, so a group's members are reached only by the route's channel. No address of anyone
// outside the unit chain is read.
func (w *Worker) unitAdminEmails(ctx context.Context, unit *uuid.UUID) ([]string, error) {
	if unit == nil {
		return nil, nil
	}
	rows, err := w.pool.Query(ctx, unitAdminEmailsSQL, *unit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (string, error) {
		var e string
		err := r.Scan(&e)
		return strings.TrimSpace(e), err
	})
}
