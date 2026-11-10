// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestStoredFanoutRecorder_NoSinkWhenTheStoreRefuses: strict mode's intent row reaches a sink only once
// the store took it. A failed store write returns its error and no sink sees the row, so a refused change
// never shows in a sink as an intent (fanoutRecorder, by contrast, sends it on).
func TestStoredFanoutRecorder_NoSinkWhenTheStoreRefuses(t *testing.T) {
	// Never reachable: the store write fails.
	pool, err := pgxpool.New(t.Context(), "postgres://u:p@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	probe := &ctxProbeSink{}
	rec := storedFanoutRecorder{pool: pool, fanout: sinks.NewFanout(probe)}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ev := types.AuditEvent{ID: uuid.New(), Action: "audit.intent.record", Outcome: "success", Time: time.Now().UTC()}
	if err := rec.Record(ctx, ev); err == nil {
		t.Fatal("the store write to an unreachable database succeeded")
	}
	if probe.emits != 0 {
		t.Fatalf("a sink got %d emits of a row the store refused; want 0", probe.emits)
	}
}

// TestStrictAuditRecorder_OffIsNil: with strict mode off there is no strict recorder, so api.Config's
// AuditStrict is nil and no intent row is written.
func TestStrictAuditRecorder_OffIsNil(t *testing.T) {
	if rec := strictAuditRecorder(false, nil, nil, nil, nil, nil); rec != nil {
		t.Fatalf("strictAuditRecorder(false) = %T, want nil", rec)
	}
}
