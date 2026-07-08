// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Compile-time assertion: Recorder implements audit.Recorder.
var _ audit.Recorder = Recorder{}

// Recorder wraps a pool and implements audit.Recorder via InsertAuditEvent.
// This satisfies the assignment: "internal/store implements audit.Recorder
// (Record == InsertAuditEvent)".
type Recorder struct {
	Pool *pgxpool.Pool
}

// Record appends ev to the append-only audit_events table.
func (rec Recorder) Record(ctx context.Context, ev types.AuditEvent) error {
	return InsertAuditEvent(ctx, rec.Pool, ev)
}
