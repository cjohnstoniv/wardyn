// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/secretstore/subjectkey/subjectkeytest"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A replay of an event the table already holds reaches the sinks with the stored row's hashes, not empty ones.
// The existing landedRecorder tests run on a closed pool and so cannot reach a store without Postgres.
func TestPG_LandedRecorderEmitsTheStoredHashesForAReplayedDuplicate(t *testing.T) {
	pool := subjectkeytest.ThrowawayDB(t)
	sink := &captureSink{}
	rec := landedRecorder{primary: store.Recorder{Pool: pool}, fanout: sinks.NewFanout(sink)}
	ev := types.AuditEvent{ID: uuid.New(), Time: time.Now().UTC().Truncate(time.Microsecond), ActorType: types.ActorSystem,
		Actor: "wardynd", Action: "approval.decide", Outcome: "success"}

	stored := ev
	if err := store.InsertAuditEvent(t.Context(), pool, &stored); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := rec.Record(store.WithAuditReplay(t.Context()), ev); err != nil {
		t.Fatalf("replayed write: %v", err)
	}
	got := sink.events()
	if len(got) != 1 {
		t.Fatalf("the sink received %d events, want 1", len(got))
	}
	if got[0].RowHash == "" || got[0].RowHash != stored.RowHash || got[0].PrevHash != stored.PrevHash {
		t.Errorf("the sink's hashes %q/%q, want the stored %q/%q", got[0].PrevHash, got[0].RowHash, stored.PrevHash, stored.RowHash)
	}
}
