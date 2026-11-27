// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The cancel of a booking the sweep made and may no longer keep (#3458). A cancel that fails is recorded
// cancel_pending:<reason> and retried each tick, so the slot is never recorded cancelled while its run waits.

// cancelBookedRun kills the one run a booking made, while it still waits, as the system, with reason, and reports
// whether that run is stopped: killed now, ended before it started, or gone. A run that started keeps its own end
// (false, nil): the kill's claim is QUEUED to KILLED only (cancelQueuedRunsOf). A run read or kill that failed is
// (false, err), and claims nothing, for the sweep to try again; a lost claim reads the run once more.
func (s *Server) cancelBookedRun(ctx context.Context, runID uuid.UUID, reason string) (bool, error) {
	for range 2 {
		run, err := s.cfg.Store.GetRun(ctx, runID)
		switch {
		case errors.Is(err, store.ErrNotFound):
			return true, nil
		case err != nil:
			return false, err
		case run.State.IsTerminal() && run.AdmittedAt == nil:
			return true, nil
		case run.State != types.RunQueued:
			return false, nil
		}
		applied, _, err := s.killRunCascade(ctx, run, types.ActorSystem, "wardynd", map[string]any{"reason": reason})
		if err != nil || applied {
			return applied, err
		}
	}
	return false, errors.New("api: a scheduled booking's kill lost its claim twice")
}

// scheduleKillReason is the kill's reason for a booking cancelled with reason. One whose owner may no longer act is
// killed session_revoked, as their queued runs are when their credentials are revoked (cancelQueuedRunsOf).
func scheduleKillReason(reason string) string {
	if reason == schedulePausedIdentity {
		return queueCancelSessionRevoked
	}
	return reason
}

// scheduleCancelRecord is a slot's outcome and its run.schedule.fire row once the cancel of its booking with reason
// has an answer: cancelled with a failure row when the run is stopped, or booked with a success row naming the
// cancel as too late when the run started.
func scheduleCancelRecord(reason string, runID uuid.UUID, dueAt time.Time, stopped bool) (outcome, result string, data map[string]any) {
	if stopped {
		return scheduleOutcomeCancelled + reason, "failure", map[string]any{"reason": reason, "run_id": runID, "due_at": dueAt}
	}
	return scheduleOutcomeBooked, "success", map[string]any{"run_id": runID, "due_at": dueAt, "cancel_too_late": reason}
}

// dropAdvance moves the schedule past the slot once the cancel of its booking with reason has answered
// (scheduleCancelRecord). A cancel that failed (err) moves it recorded cancel_pending:<reason> with no fire row, for
// the next tick to retry (retryCancels).
func (sw *scheduleSweep) dropAdvance(ctx context.Context, sc store.RunSchedule, rule scheduleRule, runID uuid.UUID, reason string, stopped bool, err error) {
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: run schedule booking not cancelled; the next tick tries again", slog.String("schedule", sc.ID.String()),
			slog.String("run_id", runID.String()), slog.Any("err", err))
		sw.advance(ctx, sc, rule, &runID, scheduleOutcomeCancelPending+reason)
		return
	}
	outcome, result, data := scheduleCancelRecord(reason, runID, sc.NextAt, stopped)
	if sw.advance(ctx, sc, rule, &runID, outcome) {
		sw.fire(ctx, sc, &runID, result, data)
	}
}

// retryCancels retries the cancel of every booking recorded cancel_pending:<reason>, paused or not, and settles each
// answered one with a compare-and-set on that pending outcome (scheduleCancelRecord). Only the sweep whose settle won
// writes the slot's run.schedule.fire row. A cancel that fails again stays pending for the next tick.
func (sw *scheduleSweep) retryCancels(ctx context.Context) {
	pending, err := sw.st.PendingRunScheduleCancels(ctx, scheduleOutcomeCancelPending, scheduleSweepBatch)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: pending run schedule cancels unread", slog.Any("err", err))
		return
	}
	for _, sc := range pending {
		if sc.LastRunID == nil || sc.LastDueAt == nil {
			continue // a pending cancel is always recorded with its run and slot (dropAdvance)
		}
		runID, reason := *sc.LastRunID, strings.TrimPrefix(sc.LastOutcome, scheduleOutcomeCancelPending)
		stopped, err := sw.cancel(ctx, runID, scheduleKillReason(reason))
		if err != nil {
			slog.ErrorContext(ctx, "wardynd: run schedule booking still not cancelled", slog.String("schedule", sc.ID.String()),
				slog.String("run_id", runID.String()), slog.Any("err", err))
			continue
		}
		outcome, result, data := scheduleCancelRecord(reason, runID, *sc.LastDueAt, stopped)
		won, err := sw.st.SettleRunScheduleCancel(ctx, sc.ID, runID, sc.LastOutcome, outcome)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: run schedule cancel not settled", slog.String("schedule", sc.ID.String()), slog.Any("err", err))
			continue
		}
		if won {
			sw.fire(ctx, sc, &runID, result, data)
		}
	}
}
