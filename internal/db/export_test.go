// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package db

// PartialSchemaPool is partialSchemaPool for the external db_test package,
// whose upgrade tests read through internal/store (which imports this package).
var PartialSchemaPool = partialSchemaPool
