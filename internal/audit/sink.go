// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package audit defines the append-only audit contract. The Postgres store
// is the system of record; sinks fan events out (OTLP, syslog, SIEM).
// Audit is free forever — never gate it.
package audit

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Recorder persists events to the system of record (append-only).
type Recorder interface {
	Record(ctx context.Context, ev types.AuditEvent) error
}

// Sink streams recorded events to an external destination. Sinks must not
// block recording: failures are logged and retried, never dropped silently
// without a counter.
type Sink interface {
	Name() string // "otlp" | "syslog" | ...
	Emit(ctx context.Context, ev types.AuditEvent) error
}

// LogWriteFailure reports a dropped audit write on the structured logger. A
// failed Record must never block the operation that produced the event (the
// Recorder owns durability/retry), but it must also never be swallowed in
// silence: the audit log is the system of record, so a lost event is an
// ERROR-level fact an operator can alert on.
//
// Only the event's routing fields are logged. ev.Data is deliberately NOT
// logged: it carries per-action payloads (grant scopes, jti, approval ids) and
// is the one field on an AuditEvent that can hold sensitive material. Callers
// must not add it, nor any secret-bearing attr, here.
func LogWriteFailure(ctx context.Context, ev types.AuditEvent, err error) {
	slog.ErrorContext(ctx, "audit write failed",
		slog.String("action", ev.Action),
		slog.String("target", ev.Target),
		slog.String("outcome", ev.Outcome),
		slog.Any("err", err),
	)
}
