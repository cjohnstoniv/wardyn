// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package audit defines the append-only audit contract. The Postgres store
// is the system of record; sinks fan events out (OTLP, syslog, SIEM).
// Audit is free forever — never gate it.
package audit

import (
	"context"

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
