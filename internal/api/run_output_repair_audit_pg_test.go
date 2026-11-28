// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// refusingRecorder refuses every event while refuse is set, else passes it on.
type refusingRecorder struct {
	inner  audit.Recorder
	refuse atomic.Bool
}

func (r *refusingRecorder) Record(ctx context.Context, ev types.AuditEvent) error {
	if r.refuse.Load() {
		return errors.New("the audit store refused the row")
	}
	return r.inner.Record(ctx, ev)
}

// replacedOwing writes run's incomplete row and then a complete one over it straight to the store, as a finaliser
// that stopped between its commit and its audit append leaves them, and returns the event the replacement owes.
func replacedOwing(t *testing.T, st store.PG, run uuid.UUID) store.RunOutputRepairAudit {
	t.Helper()
	if err := st.InsertPendingRunOutput(t.Context(), run); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveFinalRunOutput(t.Context(), store.RunOutput{RunID: run, Output: []byte("part\n"), Incomplete: true, Source: "stdout", MaskScope: "run"}); err != nil {
		t.Fatal(err)
	}
	wctx, rep := store.WithReplacedIncomplete(t.Context())
	if err := st.SaveFinalRunOutput(wctx, store.RunOutput{RunID: run, Output: []byte("part\nand the rest\n"), Source: "stdout", MaskScope: "run"}); err != nil {
		t.Fatal(err)
	}
	if !rep.Replaced || rep.Owed.EventID == uuid.Nil {
		t.Fatalf("the replacement reported %+v, want a replaced row and its owed event", rep)
	}
	return rep.Owed
}

// eventsWithID counts the audit events carrying id.
func eventsWithID(l *maskLab, id uuid.UUID) int {
	n := 0
	for _, ev := range l.rec.snapshot() {
		if ev.ID == id {
			n++
		}
	}
	return n
}

// A replacement whose finaliser stopped after the commit and before the audit append still gets its event: the
// next sweep writes exactly one, with the id the replacement fixed, and a later sweep writes none.
func TestRepairAuditOwedAcrossAStop_PG(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	run := l.run()
	st := store.NewPG(l.pool)
	owed := replacedOwing(t, st, run.ID)
	if n := eventsWithID(l, owed.EventID); n != 0 {
		t.Fatalf("%d events before any sweep, want 0", n)
	}
	for range 2 {
		if err := rp.srv.settleRepairAudits(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := eventsWithID(l, owed.EventID); n != 1 {
		t.Fatalf("%d events with the owed id after two sweeps, want 1", n)
	}
	if n := replacedAudits(t, l, run.ID); n != 1 {
		t.Fatalf("%d replacement audits for the run, want 1", n)
	}
	if left, err := st.OwedRunOutputRepairAudits(t.Context(), 10); err != nil || len(left) != 0 {
		t.Fatalf("owed after the sweep: %v %v, want none", left, err)
	}
}

// A refused append leaves the event owed; once the recorder accepts, the event is written once and settled.
func TestRepairAuditStaysOwedWhileRefused_PG(t *testing.T) {
	l := newMaskLab(t)
	rp := l.replica()
	rec := &refusingRecorder{inner: l.rec}
	rec.refuse.Store(true)
	rp.srv.cfg.Audit = rec
	run := l.run()
	st := store.NewPG(l.pool)
	owed := replacedOwing(t, st, run.ID)
	if err := rp.srv.settleRepairAudits(t.Context()); err != nil {
		t.Fatal(err)
	}
	if left, err := st.OwedRunOutputRepairAudits(t.Context(), 10); err != nil || len(left) != 1 || left[0].EventID != owed.EventID {
		t.Fatalf("owed after a refused append: %v %v, want the one event", left, err)
	}
	rec.refuse.Store(false)
	for range 2 {
		if err := rp.srv.settleRepairAudits(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
	if n := eventsWithID(l, owed.EventID); n != 1 {
		t.Fatalf("%d events with the owed id once accepted, want 1", n)
	}
	if left, err := st.OwedRunOutputRepairAudits(t.Context(), 10); err != nil || len(left) != 0 {
		t.Fatalf("owed after the accepted append: %v %v, want none", left, err)
	}
}
