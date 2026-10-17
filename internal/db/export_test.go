// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PartialSchemaPool is partialSchemaPool for the external db_test package,
// whose upgrade tests read through internal/store (which imports this package).
var PartialSchemaPool = partialSchemaPool

// ClosedEnumValues is the closedEnumChecks set for table.column, for the
// external db_test package; nil when the case table has no entry for it.
func ClosedEnumValues(table, column string) map[string]bool {
	for _, c := range closedEnumChecks() {
		if c.table == table && c.column == column {
			return c.known
		}
	}
	return nil
}
