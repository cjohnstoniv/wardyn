// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// BatchRecorder is a Recorder that records several events in one call, so the store appends them under
// one lock. A batch row is stamped, masked, sealed, spooled and sent to the sinks exactly as a single one.
type BatchRecorder interface {
	Recorder
	RecordBatch(ctx context.Context, evs []types.AuditEvent) error
}

// RecordBatch hands evs to rec in one call when rec batches, else records each in turn (noting each event
// Appended, Spooled or Refused from its error). The result is nil when every event was stored; an error
// wrapping ErrSpooled when none was lost but some wait in the spool; otherwise the first error that lost one.
func RecordBatch(ctx context.Context, rec Recorder, evs []types.AuditEvent) error {
	if len(evs) == 0 {
		return nil
	}
	if br, ok := rec.(BatchRecorder); ok {
		return br.RecordBatch(ctx, evs)
	}
	var res batchResult
	for _, ev := range evs {
		res.note(ctx, ev.ID, rec.Record(ctx, ev))
	}
	return res.err()
}

// batchResult folds the errors of several records into RecordBatch's result: the first error that lost an
// event, else the first that spooled one, else nil.
type batchResult struct{ lost, spooled error }

// note notes id from err, the error one Record gave it: nil is appended, ErrSpooled spooled, any other refused.
func (r *batchResult) note(ctx context.Context, id uuid.UUID, err error) {
	switch {
	case err == nil:
		NoteBatch(ctx, id, BatchAppended)
	case errors.Is(err, ErrSpooled):
		NoteBatch(ctx, id, BatchSpooled)
	default:
		NoteBatch(ctx, id, BatchRefused)
	}
	r.add(err)
}

// add folds err, the result of one record or of a whole inner batch, without noting any event.
func (r *batchResult) add(err error) {
	switch {
	case err == nil:
	case errors.Is(err, ErrSpooled):
		if r.spooled == nil {
			r.spooled = err
		}
	default:
		if r.lost == nil {
			r.lost = err
		}
	}
}

// err is the folded result.
func (r *batchResult) err() error {
	if r.lost != nil {
		return r.lost
	}
	return r.spooled
}

// BatchStatus is what became of one event of a batch. The zero value is "not kept".
type BatchStatus uint8

const (
	BatchUnsettled BatchStatus = iota // no stage settled it: treated as not kept
	BatchAppended                     // appended on this call
	BatchHeld                         // already stored by an earlier call (replay read-back)
	BatchSpooled                      // held by the audit spool or the coalescer, written later
	BatchRefused                      // refused by a stage, or lost by both the store and the spool
)

// BatchOutcome collects, per event id, what each stage did with a batch. Safe for concurrent use; a later
// note for an id replaces an earlier one.
type BatchOutcome struct {
	mu       sync.Mutex
	m        map[uuid.UUID]BatchStatus
	deferred []func(context.Context)
}

type batchOutcomeKey struct{}

// WithBatchOutcome returns ctx carrying a new BatchOutcome, which every stage of the chain notes into.
func WithBatchOutcome(ctx context.Context) (context.Context, *BatchOutcome) {
	o := &BatchOutcome{m: map[uuid.UUID]BatchStatus{}}
	return context.WithValue(ctx, batchOutcomeKey{}, o), o
}

func batchOutcomeFrom(ctx context.Context) *BatchOutcome {
	o, _ := ctx.Value(batchOutcomeKey{}).(*BatchOutcome)
	return o
}

// NoteBatch notes what became of event id. A no-op when ctx carries no outcome.
func NoteBatch(ctx context.Context, id uuid.UUID, st BatchStatus) {
	o := batchOutcomeFrom(ctx)
	if o == nil {
		return
	}
	o.mu.Lock()
	o.m[id] = st
	o.mu.Unlock()
}

// Status is what became of event id; BatchUnsettled when no stage noted it.
func (o *BatchOutcome) Status(id uuid.UUID) BatchStatus {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.m[id]
}

// DeferEmit holds a sink emit until the door knows whether this attempt is the one that lands the row. With
// no outcome in ctx (every caller but the batch door) it runs fn at once, as today.
func DeferEmit(ctx context.Context, fn func(context.Context)) {
	o := batchOutcomeFrom(ctx)
	if o == nil {
		fn(ctx)
		return
	}
	o.mu.Lock()
	o.deferred = append(o.deferred, fn)
	o.mu.Unlock()
}

// Release runs the held emits when emit is true and drops them otherwise. Called once, by the door.
func (o *BatchOutcome) Release(ctx context.Context, emit bool) {
	o.mu.Lock()
	fns := o.deferred
	o.deferred = nil
	o.mu.Unlock()
	if !emit {
		return
	}
	for _, fn := range fns {
		fn(ctx)
	}
}

var (
	_ BatchRecorder = DelegationRecorder{}
	_ BatchRecorder = DryRunRecorder{}
	_ BatchRecorder = PolicyStampRecorder{}
	_ BatchRecorder = (*DenialCoalescer)(nil)
)

// RecordBatch stamps each event as Record does and hands the batch on.
func (r DelegationRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	out := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		ev.Data = StampSponsor(ctx, StampDelegation(ctx, ev.Data))
		ev.Data = StampProxyPeer(ctx, ev.SourceIP, ev.Data)
		out[i] = ev
	}
	return RecordBatch(ctx, r.Inner, out)
}

// RecordBatch stamps each event as Record does and hands the batch on.
func (r DryRunRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	out := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		ev.Data = StampDryRun(ctx, ev.Data)
		out[i] = ev
	}
	return RecordBatch(ctx, r.Inner, out)
}

// RecordBatch stamps each event as Record does, with one Lookup per distinct run of the batch, and hands the
// batch on. A run whose Lookup fails has its rows recorded without the keys, as Record records one.
func (r PolicyStampRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	type looked struct {
		st  types.RunPolicyState
		err error
	}
	runs := map[uuid.UUID]looked{}
	out := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		var stamp types.PolicyStamp
		if ev.RunID != nil && r.Lookup != nil {
			l, ok := runs[*ev.RunID]
			if !ok {
				l.st, l.err = r.Lookup(ctx, *ev.RunID)
				runs[*ev.RunID] = l
			}
			if l.err != nil {
				if now := time.Now().UnixNano(); now-lastPolicyStampWarn.Load() > int64(policyStampWarnEvery) {
					lastPolicyStampWarn.Store(now)
					slog.Warn("audit: run policy digest unreadable, row recorded without policy_digest", slog.String("action", ev.Action), slog.Any("err", l.err))
				}
			} else {
				stamp = PolicyStampFor(ctx, *ev.RunID, l.st)
			}
		}
		ev.Data = StampPolicy(ev.Data, stamp)
		out[i] = ev
	}
	return RecordBatch(ctx, r.Inner, out)
}

// RecordBatch passes each event Record would coalesce through Record, one by one (the first of a window is a
// real write), noting it from Record's error, and hands the rest on as one batch.
func (c *DenialCoalescer) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	if !DryRunFrom(ctx) {
		return RecordBatch(ctx, c.Inner, evs)
	}
	var res batchResult
	rest := make([]types.AuditEvent, 0, len(evs))
	for _, ev := range evs {
		if ev.Action != DenialAction || ev.Outcome != DenialOutcome {
			rest = append(rest, ev)
			continue
		}
		if err := c.Record(ctx, ev); err == nil {
			NoteBatch(ctx, ev.ID, BatchSpooled) // kept now, or counted into the window's summary
		} else {
			res.note(ctx, ev.ID, err)
		}
	}
	res.add(RecordBatch(ctx, c.Inner, rest))
	return res.err()
}
