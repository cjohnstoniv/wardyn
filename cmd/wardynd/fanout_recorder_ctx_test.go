// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ctxProbeSink records whether the context it was emitted under was cancelled.
type ctxProbeSink struct {
	emits    int
	ctxErrAt error
}

func (*ctxProbeSink) Name() string { return "probe" }

func (p *ctxProbeSink) Emit(ctx context.Context, _ types.AuditEvent) error {
	p.emits++
	p.ctxErrAt = ctx.Err()
	return nil
}

// A row recorded under a cancelled context (a client that left, a shutdown)
// still reaches the sinks, on a context that is not cancelled (#2310).
func TestFanoutRecorder_EmitsToSinksWhenTheCallerIsGone(t *testing.T) {
	// Never dialled: the cancelled context fails the store write first.
	pool, err := pgxpool.New(t.Context(), "postgres://u:p@127.0.0.1:1/db")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	probe := &ctxProbeSink{}
	rec := fanoutRecorder{primary: store.Recorder{Pool: pool}, fanout: sinks.NewFanout(probe)}
	gone, cancel := context.WithCancel(context.Background())
	cancel()
	if err := rec.Record(gone, types.AuditEvent{ID: uuid.New(), Action: "fanout.test", Time: time.Now().UTC()}); err == nil {
		t.Fatal("the store write on a cancelled context succeeded")
	}
	if probe.emits != 1 || probe.ctxErrAt != nil {
		t.Fatalf("sink emits = %d under ctx error %v; want 1 emit on a live context", probe.emits, probe.ctxErrAt)
	}
}
