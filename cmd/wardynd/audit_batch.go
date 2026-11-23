// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Every stage of the serving chain (buildAuditChain) takes a batch, so a batch reaches the store whole and is
// appended under one lock, and each stage treats a batch row exactly as a single one. The strict chain and the
// spool drain's recorders stay per row.
var (
	_ audit.BatchRecorder = maskingRecorder{}
	_ audit.BatchRecorder = sealingRecorder{}
	_ audit.BatchRecorder = spoolingRecorder{}
	_ audit.BatchRecorder = fanoutRecorder{}
	_ audit.BatchRecorder = store.Recorder{}
)

// RecordBatch masks each event as Record does and hands the batch on.
func (m maskingRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	out := make([]types.AuditEvent, len(evs))
	for i, ev := range evs {
		out[i] = m.prepare(ev)
	}
	return audit.RecordBatch(ctx, m.inner, out)
}

// RecordBatch seals each event as Record does. Rows that need no seal, and sealed rows that are not pending,
// go on as one batch; a pending row goes to the spool, as Record sends it. A row Record would refuse is
// logged as Record logs it, noted refused and left for the caller's retry, and the others are still stored:
// the first refusal is the result unless the batch below lost an event. Replay mode records each row in turn.
func (r sealingRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	if r.replay {
		var lost, spooled error
		for _, ev := range evs {
			err := r.record(ctx, ev)
			switch {
			case err == nil:
				audit.NoteBatch(ctx, ev.ID, audit.BatchAppended)
			case errors.Is(err, audit.ErrSpooled):
				audit.NoteBatch(ctx, ev.ID, audit.BatchSpooled)
				spooled = cmpErr(spooled, err)
			default:
				audit.NoteBatch(ctx, ev.ID, audit.BatchRefused)
				lost = cmpErr(lost, err)
			}
		}
		return cmpErr(lost, spooled)
	}
	if r.src == nil {
		return audit.RecordBatch(ctx, r.inner, evs)
	}
	var refused error
	refuse := func(ev types.AuditEvent, err error) {
		audit.NoteBatch(ctx, ev.ID, audit.BatchRefused)
		refused = cmpErr(refused, err)
	}
	below := make([]types.AuditEvent, 0, len(evs))
	for _, ev := range evs {
		if !r.src.seals(ev) {
			below = append(below, ev)
			continue
		}
		sealer := r.src.sealer.Load()
		if sealer == nil {
			slog.WarnContext(ctx, "audit row not recorded", "action", ev.Action, "why", "keys_not_ready")
			refuse(ev, fmt.Errorf("audit seal: %s not recorded: the sealing keys are not available yet and the row has personal fields; it is never written in the clear", ev.Action))
			continue
		}
		sealed, pending, err := sealer.Seal(ctx, ev)
		if err != nil {
			slog.WarnContext(ctx, "audit row not recorded", "action", ev.Action, "why", "seal_refused")
			refuse(ev, fmt.Errorf("audit seal: %s not recorded: %w", ev.Action, err))
			continue
		}
		if !pending {
			below = append(below, sealed)
			continue
		}
		if r.spool == nil {
			slog.WarnContext(ctx, "audit row not recorded", "action", ev.Action, "why", "no_spool")
			refuse(ev, fmt.Errorf("audit seal: %s not recorded: a subject key could not be had and there is no spool to hold the row", ev.Action))
			continue
		}
		slog.WarnContext(ctx, "wardynd: a subject's audit key could not be had; the row is held in the audit spool sealed under the pending key and is re-sealed by the drain",
			slog.String("action", ev.Action))
		if err := r.spool.Append(sealed); err != nil {
			refuse(ev, err)
			continue
		}
		audit.NoteBatch(ctx, ev.ID, audit.BatchSpooled)
	}
	err := audit.RecordBatch(ctx, r.inner, below)
	if err != nil && !errors.Is(err, audit.ErrSpooled) {
		return err
	}
	return cmpErr(refused, err)
}

// RecordBatch hands the batch on and, when the write below fails, logs it once and spools every event, as
// Record spools one. An event the spool cannot take is lost: it is logged, noted refused, and the store's
// error is the result.
func (r spoolingRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	err := audit.RecordBatch(ctx, r.inner, evs)
	if err == nil || len(evs) == 0 {
		return nil
	}
	slog.ErrorContext(ctx, "wardynd: AUDIT WRITE FAILED",
		slog.String("action", evs[0].Action),
		slog.Int("count", len(evs)),
		slog.Any("err", err),
	)
	if r.spool == nil {
		for _, ev := range evs {
			audit.NoteBatch(ctx, ev.ID, audit.BatchRefused)
		}
		return err
	}
	lost := false
	for _, ev := range evs {
		if ferr := r.spool.Append(ev); ferr != nil {
			slog.ErrorContext(ctx, "wardynd: AUDIT FALLBACK SPOOL FAILED (EVENT LOST)",
				slog.String("action", ev.Action),
				slog.Any("err", ferr),
			)
			audit.NoteBatch(ctx, ev.ID, audit.BatchRefused)
			lost = true
			continue
		}
		audit.NoteBatch(ctx, ev.ID, audit.BatchSpooled)
	}
	if lost {
		return err
	}
	return fmt.Errorf("%w: %w", audit.ErrSpooled, err)
}

// RecordBatch appends the batch under one lock and emits to the sinks, at once, only the events appended on
// this call, with the hashes the database computed: a retried batch does not send a row the sinks already
// have. When the store fails, each event's hash-less emit, the one Record sends, is held (audit.DeferEmit)
// until the caller knows whether this attempt is the one whose rows the spool keeps.
func (f fanoutRecorder) RecordBatch(ctx context.Context, evs []types.AuditEvent) error {
	cp := append([]types.AuditEvent(nil), evs...)
	ptrs := make([]*types.AuditEvent, len(cp))
	for i := range cp {
		ptrs[i] = &cp[i]
	}
	held, err := store.InsertAuditEvents(ctx, f.primary.Pool, ptrs)
	if err != nil {
		if f.fanout != nil {
			for _, ev := range cp {
				audit.DeferEmit(ctx, func(ctx context.Context) { f.emit(ctx, ev) })
			}
		}
		return err
	}
	for i, ev := range cp {
		if held[i] {
			audit.NoteBatch(ctx, ev.ID, audit.BatchHeld)
			continue
		}
		audit.NoteBatch(ctx, ev.ID, audit.BatchAppended)
		if f.fanout != nil {
			f.emit(ctx, ev)
		}
	}
	return nil
}

// emit sends ev to the sinks as Record does: detached from the caller, a failure logged and not returned.
func (f fanoutRecorder) emit(ctx context.Context, ev types.AuditEvent) {
	if ferr := f.fanout.Emit(context.WithoutCancel(ctx), ev); ferr != nil {
		slog.ErrorContext(ctx, "wardynd: audit fanout emit failed",
			slog.String("action", ev.Action),
			slog.Any("err", ferr),
		)
	}
}

// cmpErr is first when it is set, else second.
func cmpErr(first, second error) error {
	if first != nil {
		return first
	}
	return second
}
