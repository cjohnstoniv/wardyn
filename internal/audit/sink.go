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
// block recording; failures are logged and retried, never silently dropped.
type Sink interface {
	Name() string // "otlp" | "syslog" | ...
	Emit(ctx context.Context, ev types.AuditEvent) error
}

// LogWriteFailure reports a dropped audit write at ERROR level so an
// operator can alert on it; it must never block the caller.
//
// ev.Data is deliberately NOT logged: it can carry sensitive per-action
// payloads (grant scopes, jti, approval ids). Callers must not add it here.
func LogWriteFailure(ctx context.Context, ev types.AuditEvent, err error) {
	slog.ErrorContext(ctx, "audit write failed",
		slog.String("action", ev.Action),
		slog.String("target", ev.Target),
		slog.String("outcome", ev.Outcome),
		slog.Any("err", err),
	)
}
