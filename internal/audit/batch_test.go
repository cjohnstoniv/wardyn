// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// errorsByID answers Record with the error set for the event's id, and keeps every event.
type errorsByID struct {
	errs map[uuid.UUID]error
	got  []types.AuditEvent
}

func (r *errorsByID) Record(_ context.Context, ev types.AuditEvent) error {
	r.got = append(r.got, ev)
	return r.errs[ev.ID]
}

func batchRows(n int) []types.AuditEvent {
	out := make([]types.AuditEvent, n)
	for i := range out {
		out[i] = types.AuditEvent{ID: uuid.New(), Action: "test.batch", Outcome: "success"}
	}
	return out
}

// TestRecordBatch_FallsBackPerRow: a recorder that does not batch gets each event in turn, each noted from its
// error, and the result is the first lost error, else the first spooled one, else nil.
func TestRecordBatch_FallsBackPerRow(t *testing.T) {
	evs := batchRows(4)
	lost := errors.New("store down, spool down")
	spooled := fmt.Errorf("%w: store down", ErrSpooled)
	rec := &errorsByID{errs: map[uuid.UUID]error{evs[1].ID: spooled, evs[2].ID: lost}}
	ctx, out := WithBatchOutcome(context.Background())
	if err := RecordBatch(ctx, rec, evs); !errors.Is(err, lost) {
		t.Fatalf("RecordBatch = %v, want the lost event's error", err)
	}
	if len(rec.got) != len(evs) {
		t.Fatalf("recorder got %d events, want each of %d", len(rec.got), len(evs))
	}
	want := []BatchStatus{BatchAppended, BatchSpooled, BatchRefused, BatchAppended}
	for i, ev := range evs {
		if rec.got[i].ID != ev.ID {
			t.Fatalf("event %d out of order", i)
		}
		if st := out.Status(ev.ID); st != want[i] {
			t.Errorf("event %d noted %d, want %d", i, st, want[i])
		}
	}
	if st := out.Status(uuid.New()); st != BatchUnsettled {
		t.Errorf("an unknown id is %d, want unsettled", st)
	}

	rec = &errorsByID{errs: map[uuid.UUID]error{evs[3].ID: spooled}}
	if err := RecordBatch(ctx, rec, evs); !errors.Is(err, ErrSpooled) {
		t.Fatalf("with one spooled and none lost: %v, want ErrSpooled", err)
	}
	if err := RecordBatch(ctx, &errorsByID{}, evs); err != nil {
		t.Fatalf("all stored: %v, want nil", err)
	}
	// No outcome in ctx: still recorded, nothing to note into.
	if err := RecordBatch(context.Background(), &errorsByID{}, evs); err != nil {
		t.Fatal(err)
	}
}

// TestBatchOutcome_ReleaseRunsOnlyOnTrue: a held emit runs on Release(ctx, true), is dropped by Release(ctx,
// false), runs at most once, and runs at once when ctx carries no outcome.
func TestBatchOutcome_ReleaseRunsOnlyOnTrue(t *testing.T) {
	ran := 0
	fn := func(context.Context) { ran++ }

	DeferEmit(context.Background(), fn)
	if ran != 1 {
		t.Fatalf("with no outcome in ctx the emit ran %d times, want at once", ran)
	}

	ctx, out := WithBatchOutcome(context.Background())
	DeferEmit(ctx, fn)
	DeferEmit(ctx, fn)
	if ran != 1 {
		t.Fatalf("a held emit ran before release (%d)", ran)
	}
	out.Release(ctx, true)
	if ran != 3 {
		t.Fatalf("after Release(true) %d emits ran, want both held", ran-1)
	}
	out.Release(ctx, true)
	if ran != 3 {
		t.Fatalf("a second release ran the emits again (%d)", ran)
	}

	ctx, out = WithBatchOutcome(context.Background())
	DeferEmit(ctx, fn)
	out.Release(ctx, false)
	out.Release(ctx, true)
	if ran != 3 {
		t.Fatalf("an emit dropped by Release(false) ran (%d)", ran)
	}
}

// TestDenialCoalescerBatch_NotesFromRecordError: under a dry run, a refused dry run in a batch goes through the
// coalescer's Record and is noted from its error (nil and a spooled error are spooled, any other refused); the
// rest of the batch is handed on as one.
func TestDenialCoalescerBatch_NotesFromRecordError(t *testing.T) {
	first, repeat, other := denial("sub-a", "runs.image", "byoi_user"), denial("sub-a", "runs.image", "byoi_user"), batchRows(1)[0]
	inner := &errorsByID{errs: map[uuid.UUID]error{}}
	c := &DenialCoalescer{Inner: inner}
	ctx, out := WithBatchOutcome(WithDryRun(context.Background()))
	if err := c.RecordBatch(ctx, []types.AuditEvent{first, other, repeat}); err != nil {
		t.Fatal(err)
	}
	for _, ev := range []types.AuditEvent{first, repeat} {
		if st := out.Status(ev.ID); st != BatchSpooled {
			t.Errorf("denial %s noted %d, want spooled", ev.ID, st)
		}
	}
	if st := out.Status(other.ID); st != BatchAppended {
		t.Errorf("the other row noted %d, want appended", st)
	}
	if len(inner.got) != 2 || inner.got[0].ID != first.ID || inner.got[1].ID != other.ID {
		t.Fatalf("below the coalescer: %v, want the first denial then the other row", inner.got)
	}

	spooledDenial, lostDenial := denial("sub-b", "x", "r1"), denial("sub-c", "y", "r2")
	inner.errs[spooledDenial.ID] = fmt.Errorf("%w: store down", ErrSpooled)
	lost := errors.New("lost")
	inner.errs[lostDenial.ID] = lost
	if err := c.RecordBatch(ctx, []types.AuditEvent{spooledDenial, lostDenial}); !errors.Is(err, lost) {
		t.Fatalf("RecordBatch = %v, want the lost denial's error", err)
	}
	if out.Status(spooledDenial.ID) != BatchSpooled || out.Status(lostDenial.ID) != BatchRefused {
		t.Fatalf("notes = %d, %d; want spooled, refused", out.Status(spooledDenial.ID), out.Status(lostDenial.ID))
	}

	// Without a dry run nothing is coalesced: the denials are one batch below.
	plain := &errorsByID{}
	c2 := &DenialCoalescer{Inner: plain}
	if err := c2.RecordBatch(context.Background(), []types.AuditEvent{denial("s", "t", "r"), denial("s", "t", "r")}); err != nil || len(plain.got) != 2 {
		t.Fatalf("outside a dry run: %v, %d rows; want both passed through", err, len(plain.got))
	}
	c.Flush(context.Background())
}
