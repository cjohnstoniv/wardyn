// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// MaxAuditTargetLen bounds the audit row's `target` column (audit_events.target
// is plain TEXT, i.e. unbounded).
//
// The target is `r.URL.Path` on the two lanes an untrusted caller drives —
// auth.failed and authz.denied — and chi matches a path of any length the
// server accepted, which is MaxHeaderBytes (1 MiB) plus slack. The
// UNAUTHENTICATED twin is rate-limited to ~1 row/sec; the AUTHENTICATED
// authz.denied lane is not limited at all, so a member looping
// `PUT /policies/<1 MiB>` writes megabytes per second into a table whose
// Postgres trigger forbids DELETE. 512 bytes holds every real target this tree
// writes (a UUID route is ~50) with an order of magnitude to spare.
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
// This satisfies the assignment: "internal/store implements audit.Recorder
// (Record == InsertAuditEvent)".
type Recorder struct {
	Pool *pgxpool.Pool
}

// Record appends ev to the append-only audit_events table.
//
// The chain hashes InsertAuditEvent fills in are DROPPED here: audit.Recorder
// takes ev by value, so there is nowhere to hand them back. A caller that wants
// the head hash — cmd/wardynd's fanoutRecorder, which forwards it to the audit
// sinks — calls InsertAuditEvent directly with its own &ev.
func (rec Recorder) Record(ctx context.Context, ev types.AuditEvent) error {
	return InsertAuditEvent(ctx, rec.Pool, &ev)
}
