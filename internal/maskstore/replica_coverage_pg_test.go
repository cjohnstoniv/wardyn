// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package maskstore

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit/sinks"
	"github.com/cjohnstoniv/wardyn/internal/maskmanifest"
	"github.com/cjohnstoniv/wardyn/internal/secretmask"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

type heldEventSink struct {
	mu sync.Mutex
	ev types.AuditEvent
}

func (*heldEventSink) Name() string { return "held" }
func (h *heldEventSink) Emit(_ context.Context, ev types.AuditEvent) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.ev = ev
	return nil
}

// TestPG_AReplicaAppliesAnErasureBeforeItsManifestsHearOfIt: replica A erases a person's mask copies on their
// own (the fence, then the registry's tombstones); replica B reads the tombstones and drops the run's value
// from its registry, and has not yet run DropFenced. An emit already in flight on B, whose correlation id is
// the dropped value, must export neither the value nor the id.
func TestPG_AReplicaAppliesAnErasureBeforeItsManifestsHearOfIt(t *testing.T) {
	pool, keys := erasurePG(t)
	ctx := t.Context()
	const secret = "zq-throwaway-two-replicas-9"
	run := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO agent_runs (id, created_by, agent, repo, confinement_class, state, spiffe_id, runner_target)
		VALUES ($1, 'sub', 'claude-code', 'a/b', 'CC2', 'RUNNING', 'spiffe://x/' || $2, 'docker')`, run, run.String()); err != nil {
		t.Fatal(err) // run_mask_manifest references the run
	}

	regA, regB := secretmask.NewRegistry(), secretmask.NewRegistry()
	New(pool, keys, regA) // attaches itself to regA as its Backend
	stB := New(pool, keys, regB)
	mA, mB := maskmanifest.New(pool, keys, regA), maskmanifest.New(pool, keys, regB)

	if err := mA.Start(ctx, run, "alice"); err != nil {
		t.Fatal(err)
	}
	// The value goes through A's registry, so its record is a mask_values row: the table an erasure
	// tombstones and B's read applies. A manifest rendering (mA.Append) lives in run_mask_values only,
	// which no tombstone reaches; B lets go of that one at DropFenced.
	if err := regA.Add(run, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	if err := mA.Complete(ctx, run); err != nil {
		t.Fatal(err)
	}
	if err := stB.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if !mB.Covered(ctx, run) || !mB.Held(run) {
		t.Fatal("replica B does not hold the run's manifest before the erasure; the rest proves nothing")
	}
	if bytes.Contains(regB.Masker(run).Mask([]byte(secret)), []byte(secret)) {
		t.Fatal("replica B does not mask the value before the erasure")
	}

	inside, release := make(chan struct{}), make(chan struct{})
	sink := &heldEventSink{}
	fan := sinks.NewFanout(sink)
	fan.EnrichRuns(func(context.Context, uuid.UUID) (string, bool, error) {
		close(inside)
		<-release
		return secret, true, nil
	}, sinks.Witnessed(func(id uuid.UUID, b []byte) []byte { return regB.JSONVariantMasker(id).Mask(b) }, mB.Held, regB.Generation), mB.Held)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ev := types.AuditEvent{ID: uuid.New(), RunID: &run, Action: "run.kill", Outcome: "success", Data: json.RawMessage(`{"k":1}`)}
		_ = fan.Emit(ctx, ev)
	}()
	<-inside

	// Replica A: mask_copies on its own. The column that holds the id is not touched.
	if _, err := mA.FenceSubject(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	if _, err := regA.EraseOwner(ctx, "alice"); err != nil {
		t.Fatal(err)
	}
	// Replica B: reads the tombstones. No DropFenced.
	if err := stB.Fresh(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	if mB.Held(run) {
		t.Error("replica B still vouches for the run after the tombstone dropped its value")
	}
	close(release)
	<-done

	sink.mu.Lock()
	defer sink.mu.Unlock()
	if strings.Contains(string(sink.ev.Data), secret) || strings.Contains(string(sink.ev.Data), sinks.CorrelationKey) {
		t.Errorf("the sink got %s", sink.ev.Data)
	}
}
