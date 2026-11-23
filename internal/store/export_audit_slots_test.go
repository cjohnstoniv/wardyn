// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import "github.com/jackc/pgx/v5/pgxpool"

// AuditAppendSlotCount is how many request-path writers may wait on the chain lock at once for pool.
func AuditAppendSlotCount(pool *pgxpool.Pool) int { return cap(auditAppendSlots(pool)) }
