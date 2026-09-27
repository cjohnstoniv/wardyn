// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MaxAuditTargetLen bounds the audit row's `target` column (plain TEXT,
// unbounded). An authenticated caller looping a huge path via authz.denied
// (unrate-limited, unlike auth.fail) could otherwise write megabytes per
// second into a table whose Postgres trigger forbids DELETE; 512 bytes holds
// every real target this tree writes with an order of magnitude to spare.
const MaxAuditTargetLen = 512

// AuditTargetTruncatedMarker is appended to a capped target so the row says
// what happened to it. Without it a truncated path reads as a real one, and an
// operator matching `target = '/api/v1/policies/aaa…'` would believe it.
const AuditTargetTruncatedMarker = "…[truncated]"

// CapAuditTarget bounds one audit target. A target already within the cap is
// returned BYTE-IDENTICAL: audit queries match this column exactly, so the cap
// must be invisible to every row that is not abusive.
func CapAuditTarget(target string) string {
	if len(target) <= MaxAuditTargetLen {
		return target
	}
	// Byte slice, not runes: the column is bytes and the attacker chooses the
	// encoding. A split multi-byte rune inside a truncated attacker-supplied
	// path is not a correctness problem — the marker says the value is partial.
	return target[:MaxAuditTargetLen] + AuditTargetTruncatedMarker
}

// Compile-time assertion: Recorder implements audit.Recorder.
var _ audit.Recorder = Recorder{}

// Recorder wraps a pool and implements audit.Recorder via InsertAuditEvent.
type Recorder struct {
	Pool *pgxpool.Pool
}

// Record appends ev to the append-only audit_events table. The chain hash
// InsertAuditEvent fills in is dropped here since ev is taken by value; a
// caller that wants the head hash calls InsertAuditEvent directly with &ev.
func (rec Recorder) Record(ctx context.Context, ev types.AuditEvent) error {
	return InsertAuditEvent(ctx, rec.Pool, &ev)
}
