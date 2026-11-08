// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "github.com/google/uuid"

// UnitAuditQueryForTest is the SQL and arguments the unit feed (withGovernance) or its export reads.
func UnitAuditQueryForTest(units []uuid.UUID, f AuditFilter, withGovernance bool) (string, []any) {
	return unitAuditQuery(auditCols, units, f, 0, withGovernance)
}
