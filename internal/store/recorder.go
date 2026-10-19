// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"unicode/utf8"

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
	// The column is UTF-8 text, so Postgres refuses (22021) a target with a bad
	// byte, and a refused row is replayed from the spool and refused again.
	// Valid text passes through untouched.
	if !utf8.ValidString(target) {
		target = strings.ToValidUTF8(target, "\uFFFD")
	}
	if len(target) <= MaxAuditTargetLen {
		return target
	}
	// Cut at a rune boundary: the length is in bytes, but a cut inside a
	// multi-byte rune would leave exactly the bad byte the line above removes.
	cut := MaxAuditTargetLen
	for cut > 0 && !utf8.RuneStart(target[cut]) {
		cut--
	}
	return target[:cut] + AuditTargetTruncatedMarker
}

// Compile-time assertion: Recorder implements audit.Recorder.
var _ audit.Recorder = Recorder{}

// Recorder wraps a pool and implements audit.Recorder via InsertAuditEvent.
type Recorder struct {
	Pool *pgxpool.Pool
}

// ErrAuditOutcome is Record's refusal of an outcome the audit table cannot store.
var ErrAuditOutcome = errors.New("audit outcome is not success, failure or denied")

// refusedAuditOutcomes counts Record's refusals (wardyn_audit_outcome_refused_total).
var refusedAuditOutcomes atomic.Uint64

// RefusedAuditOutcomes is how many rows Record refused for their outcome since start.
func RefusedAuditOutcomes() uint64 { return refusedAuditOutcomes.Load() }

// Record appends ev to the append-only audit_events table. The chain hash
// InsertAuditEvent fills in is dropped here since ev is taken by value; a
// caller that wants the head hash calls InsertAuditEvent directly with &ev.
//
// An outcome outside success, failure and denied is refused here, loudly (an
// error log naming the action and outcome, and a counter), rather than left to
// the table's CHECK, which a spooled retry would only meet again. It is a
// backstop: each producer's tests pin the outcomes it writes.
func (rec Recorder) Record(ctx context.Context, ev types.AuditEvent) error {
	switch ev.Outcome {
	case "success", "failure", "denied":
	default:
		refusedAuditOutcomes.Add(1)
		slog.ErrorContext(ctx, "store: audit row refused: its outcome is not one the audit table stores",
			slog.String("action", ev.Action), slog.String("outcome", ev.Outcome))
		return fmt.Errorf("%w: action %q outcome %q", ErrAuditOutcome, ev.Action, ev.Outcome)
	}
	return InsertAuditEvent(ctx, rec.Pool, &ev)
}
