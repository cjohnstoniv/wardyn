// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle implements workspace lifecycle automation: the Reaper loop
// finds RUNNING agent runs idle past their policy's AutoStopAfterSec threshold
// and stops them, emitting a "run.autostop" audit event for each.
//
// Idleness is wall-clock age of agent_runs.updated_at, reset only by a write
// through the store (state transitions, sandbox_ref updates, TouchRun). The
// proxy's decision-ingest touch is coalesced to one UPDATE per TouchDebounce
// window, so thresholdFor adds TouchDebounce as slack. CPU work inside the
// sandbox reaches the clock through TouchRun too: internal/api's pause sweep
// reads each candidate's CPU from the substrate and touches a busy one. Where
// that signal is off (no metrics-server) and for file writes, activity that
// never leaves the sandbox does NOT reset the clock, so a busy run can still
// be reaped; operators needing an unbounded session use the never-reap
// escape hatch (AutoStopAfterSec <= 0).
//
// Config.MaxAge (WARDYN_RUN_MAX_AGE) is a separate, absolute cap on a RUNNING
// run's age since creation. It is its own predicate: not ends_at (which a
// person may extend), not the idle compare-and-set, and it applies to a run
// whose policy never idle-reaps. It stops a run even with a request open,
// because bounding a hung run is its whole purpose.
//
// AutoStopAfterSec (policy auto_stop_after_sec): >0 idle timeout in seconds;
// 0 DISABLED/never reaped (default, matches docs/POLICIES.md); <0 also never
// reaped but kept distinct so an operator can express intent loudly.
package lifecycle

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/sweephealth"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// defaultInterval is how often the reaper wakes to scan for idle runs.
	defaultInterval = time.Minute

	// defaultStopTimeout bounds one StopRun call so a hung StopSandbox can't
	// stall the loop; on timeout the stop is skipped and retried next tick.
	defaultStopTimeout = 2 * time.Minute
)

// TouchDebounce covers the ingest touch's coalesced-UPDATE lag, added as slack
// in thresholdFor so debounce can never make an active run look idle.
const TouchDebounce = 30 * time.Second

// RunSummary is the minimal projection a Store must return for idle detection.
type RunSummary struct {
	ID        uuid.UUID
	CreatedAt time.Time
	UpdatedAt time.Time
	// PolicyAutoStopAfterSec: 0 or negative means never reap, positive is the
	// idle timeout in seconds. Store must JOIN to the policy table; zero is
	// intentional, not a missing join.
	PolicyAutoStopAfterSec int
}

// Store is the narrow persistence interface the Reaper requires; the real
// adapter maps agent_runs JOIN run_policies, tests supply a fake.
type Store interface {
	// ListRunningWithPolicy returns RUNNING runs with their policy's
	// auto_stop_after_sec (0 if unattached/unset), plus the store's own clock
	// read once for the whole scan. The reaper measures idleness against that
	// clock, not its own: UpdatedAt is Postgres-stamped, so wardynd clock skew
	// vs. the database eats directly into TouchDebounce's 30s margin — enough
	// skew stops an actively-attached run and revokes its credentials, or
	// (skewed the other way) never reaps at all. A ZERO returned time means
	// the store has no clock (in-memory doubles), and the reaper falls back
	// to its own.
	ListRunningWithPolicy(ctx context.Context) ([]RunSummary, time.Time, error)
}

// StopOutcome is what StopRun reports back to the Reaper beyond the raw error.
type StopOutcome struct {
	// Applied is false when a concurrent kill/complete already moved the run
	// terminal, or the idleness guard no-op'd because updated_at advanced past
	// the snapshot (an active attach). Either way, no spurious run.autostop.
	Applied bool
	// Errors, when non-empty, are post-transition teardown/revocation failures
	// ("identity_error"/"broker_error": creds may still be live;
	// "teardown_error": sandbox may still be routable) that leave the run not
	// fully contained. Emitted as a distinct run.revoke/failure audit event,
	// mirroring handleKillRun/revokeRunCascade. Nil on a clean stop.
	Errors map[string]string
}

// Stopper stops a single run: transitions RUNNING->STOPPED guarded on the
// snapshot's idleness, then calls runner.StopSandbox + the revoke cascade.
// Hidden behind this interface so lifecycle stays target-agnostic.
type Stopper interface {
	// StopRun must be idempotent: stopping an already-stopped run returns
	// ({Applied:false}, nil). notAfter is the snapshot's updated_at; the
	// transition is conditional on updated_at not having advanced past it, so
	// a run touched by an active attach since the snapshot is NOT stopped. A
	// non-nil error means the stop failed outright and the reaper logs/skips.
	StopRun(ctx context.Context, runID uuid.UUID, notAfter time.Time) (StopOutcome, error)

	// StopRunMaxAge stops a run that has outlived Config.MaxAge: the same
	// RUNNING->STOPPED transition and teardown as StopRun, guarded on the run's
	// created_at being at or before createdNotAfter instead of on idleness (and
	// ignoring open requests). Idempotent like StopRun.
	StopRunMaxAge(ctx context.Context, runID uuid.UUID, createdNotAfter time.Time) (StopOutcome, error)
}

// Recorder matches audit.Recorder exactly so a store.Recorder can be passed directly.
type Recorder interface {
	Record(ctx context.Context, ev types.AuditEvent) error
}

// Config holds optional overrides for Reaper behaviour; zero value is valid.
type Config struct {
	// Interval is how often the reaper scans. Default: 1 minute.
	Interval time.Duration
	// MaxAge, when positive, ends any RUNNING run created longer ago than this
	// (WARDYN_RUN_MAX_AGE). Zero or negative is off.
	MaxAge time.Duration
	// Now overrides the wall clock. Nil means use real time.
	Now func() time.Time
	// TickLock, when non-nil, makes each tick single-flight across control
	// planes: TRY a cluster-wide lock, return a release func, or (nil, false)
	// if held elsewhere — then the tick is skipped, not queued. wardynd wires
	// a Postgres try-advisory-lock; kept as a func so lifecycle has no DB
	// dependency. Nil = ungated (single-process default; what tests use).
	TickLock func(ctx context.Context) (release func(), ok bool)
	// Sweeps, when non-nil, records each tick that does real work (the one that
	// won TickLock) as the idle_reaper sweep: an attempt when it starts and a
	// success only when it finishes without error. Nil records nothing.
	Sweeps *sweephealth.Tracker
}

// Reaper is the idle-workspace garbage collector: a periodic loop that finds
// RUNNING workspaces idle past their policy threshold and stops them.
// Reaper.Run blocks until ctx is cancelled; a stopper error is logged and
// skipped so one broken sandbox never blocks the rest of the tick.
type Reaper struct {
	store    Store
	stopper  Stopper
	recorder Recorder
	now      func() time.Time
	interval time.Duration
	maxAge   time.Duration
	tickLock func(ctx context.Context) (func(), bool)
	sweeps   *sweephealth.Tracker
	logger   *slog.Logger
}

// New constructs a Reaper. store, stopper, and recorder must be non-nil.
func New(store Store, stopper Stopper, recorder Recorder, cfg Config) *Reaper {
	r := &Reaper{
		store:    store,
		stopper:  stopper,
		recorder: recorder,
		now:      cfg.Now,
		interval: cfg.Interval,
		maxAge:   cfg.MaxAge,
		tickLock: cfg.TickLock,
		sweeps:   cfg.Sweeps,
		logger:   slog.Default().With("component", "lifecycle.reaper"),
	}
	if r.now == nil {
		r.now = time.Now
	}
	if r.interval <= 0 {
		r.interval = defaultInterval
	}
	return r
}

// Run starts the reap loop, returning when ctx is cancelled. The first tick
// fires after one full Interval so startup finishes before the first scan.
func (r *Reaper) Run(ctx context.Context) {
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.Tick(ctx)
		}
	}
}

// Tick is one reap scan, gated by Config.TickLock when wired. Exported for
// integration callers/tests to drive directly; production code uses Run. The
// error is the tick's: the scan could not list runs, or a stop failed. A tick
// another control plane holds the lock for is skipped, which is not an error.
//
// Deadline is Interval+defaultStopTimeout, not just Interval: a child
// context.WithTimeout can only shorten its parent's deadline, so budgeting at
// bare Interval would silently cap every per-stop deadline short of the full
// defaultStopTimeout the constant promises.
func (r *Reaper) Tick(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, r.interval+defaultStopTimeout)
	defer cancel()
	if r.tickLock != nil {
		release, ok := r.tickLock(ctx)
		if !ok {
			// TRY lock semantics: skip rather than queue, since the scan is
			// idempotent and the next tick is one Interval away.
			r.logger.DebugContext(ctx, "lifecycle: tick skipped (reap lock held elsewhere)")
			return nil
		}
		defer release()
	}
	return r.sweeps.Tick(ctx, sweephealth.IdleReaper, r.reap)
}

// reap is the tick body — the scan itself, with no locking of its own. It
// returns the list failure, or the stops that failed outright: a run the reaper
// cannot stop keeps its credentials live, which is what a stale idle_reaper
// sweep should say.
func (r *Reaper) reap(ctx context.Context) error {
	runs, storeNow, err := r.store.ListRunningWithPolicy(ctx)
	if err != nil {
		r.logger.ErrorContext(ctx, "lifecycle: list running runs failed", "err", err)
		return fmt.Errorf("lifecycle: list running runs: %w", err)
	}

	// Measure age against the store's clock, since UpdatedAt came from it; a
	// zero answer means no clock (in-memory doubles), which fall back to r.now().
	now := storeNow
	if now.IsZero() {
		now = r.now()
	}

	var stopErrs []error
	for _, run := range runs {
		if r.maxAge > 0 && now.Sub(run.CreatedAt) >= r.maxAge {
			r.stopMaxAge(ctx, run, now)
			continue
		}
		// AutoStopAfterSec <= 0 means never reap regardless of idle time (0 =
		// disabled default; negative = explicit unbounded-attach escape hatch).
		if run.PolicyAutoStopAfterSec <= 0 {
			continue
		}
		threshold := r.thresholdFor(run)
		idleFor := now.Sub(run.UpdatedAt)
		if idleFor < threshold {
			continue
		}

		// Bound each stop with its own deadline so one hung StopSandbox can't
		// stall the loop; a timed-out stop is simply retried next tick.
		runID := run.ID
		stopCtx, cancel := context.WithTimeout(ctx, defaultStopTimeout)
		// notAfter = snapshot's updated_at, so the transition is skipped if an
		// attach keepalive touched the run since the scan.
		out, err := r.stopper.StopRun(stopCtx, runID, run.UpdatedAt)
		cancel()
		if err != nil {
			r.logger.ErrorContext(ctx, "lifecycle: stop run failed",
				"run_id", runID,
				"idle_for", idleFor,
				"threshold", threshold,
				"err", err,
			)
			stopErrs = append(stopErrs, fmt.Errorf("lifecycle: stop run %s: %w", runID, err))
			continue
		}
		if !out.Applied {
			// No-op transition (already terminal, touched since snapshot, or an
			// open request mid-wait): do NOT emit a spurious run.autostop.
			r.logger.DebugContext(ctx, "lifecycle: stop was a no-op (already terminal, touched after snapshot, or an open request is still inside its wait)",
				"run_id", runID,
			)
			continue
		}

		r.emitAutoStop(ctx, runID, idleFor, threshold)

		// Stop won but teardown/revocation failed: run is STOPPED yet not fully
		// contained. Emit a DISTINCT event so that window is visible instead of
		// hiding behind a dishonestly-clean run.autostop. Mirrors handleKillRun.
		if len(out.Errors) > 0 {
			r.emitRevokeFailure(ctx, runID, out.Errors)
		}
	}
	return errors.Join(stopErrs...)
}

// stopMaxAge ends one run past Config.MaxAge and audits it. Same per-stop
// deadline and failure handling as the idle path; a lost compare-and-set (the
// run already ended) writes nothing.
func (r *Reaper) stopMaxAge(ctx context.Context, run RunSummary, now time.Time) {
	stopCtx, cancel := context.WithTimeout(ctx, defaultStopTimeout)
	defer cancel()
	// The cutoff is the scan's clock minus the cap, so created_at is compared
	// on the one clock that stamped it.
	out, err := r.stopper.StopRunMaxAge(stopCtx, run.ID, now.Add(-r.maxAge))
	if err != nil {
		r.logger.ErrorContext(ctx, "lifecycle: max-age stop failed", "run_id", run.ID, "err", err)
		return
	}
	if !out.Applied {
		return
	}
	r.emitMaxAgeStop(ctx, run.ID, now.Sub(run.CreatedAt))
	if len(out.Errors) > 0 {
		r.emitRevokeFailure(ctx, run.ID, out.Errors)
	}
}

// thresholdFor returns the idle threshold for a run: policy AutoStopAfterSec
// (always positive here, since reap filters <= 0 before calling this) plus
// TouchDebounce slack. Sourcing the override from policy, not workspace
// config, preserves the invariant that workspace config can only narrow
// policy, never widen it.
func (r *Reaper) thresholdFor(run RunSummary) time.Duration {
	return time.Duration(run.PolicyAutoStopAfterSec)*time.Second + TouchDebounce
}

// emitAutoStop writes a "run.autostop" audit event; failures are logged and
// swallowed since audit must never gate the stop path. threshold is the
// EFFECTIVE value (policy + TouchDebounce) the reaper actually compared
// against, not the raw policy number — see docs/POLICIES.md.
func (r *Reaper) emitAutoStop(ctx context.Context, runID uuid.UUID, idleFor, threshold time.Duration) {
	data, _ := json.Marshal(map[string]any{
		"idle_for_sec":  int64(idleFor.Seconds()),
		"threshold_sec": int64(threshold.Seconds()),
		"reason":        "idle_timeout",
	})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      r.now(),
		RunID:     &runID,
		ActorType: types.ActorSystem,
		Actor:     "wardyn/lifecycle-reaper",
		Action:    "run.autostop",
		Target:    runID.String(),
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if err := r.recorder.Record(ctx, ev); err != nil {
		r.logger.ErrorContext(ctx, "lifecycle: emit autostop audit event failed",
			"run_id", runID,
			"err", fmt.Sprintf("%v", err),
		)
	}
}

// emitMaxAgeStop writes a "run.max_age.expire" audit event; like emitAutoStop,
// a failure to record is logged and swallowed.
func (r *Reaper) emitMaxAgeStop(ctx context.Context, runID uuid.UUID, age time.Duration) {
	data, _ := json.Marshal(map[string]any{
		"age_sec":     int64(age.Seconds()),
		"max_age_sec": int64(r.maxAge.Seconds()),
		"reason":      "max_age",
	})
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      r.now(),
		RunID:     &runID,
		ActorType: types.ActorSystem,
		Actor:     "wardyn/lifecycle-reaper",
		Action:    "run.max_age.expire",
		Target:    runID.String(),
		Outcome:   "success",
		Data:      json.RawMessage(data),
	}
	if err := r.recorder.Record(ctx, ev); err != nil {
		r.logger.ErrorContext(ctx, "lifecycle: emit max-age stop audit event failed",
			"run_id", runID,
			"err", fmt.Sprintf("%v", err),
		)
	}
}

// emitRevokeFailure writes a "run.revoke"/failure audit event carrying
// per-target teardown/revocation errors from a stop that reached STOPPED but
// didn't fully contain the run. Mirrors handleKillRun/revokeRunCascade so a
// silently-failed revoke (token live until its <=1h TTL, or sandbox routable)
// stays visible instead of hiding behind run.autostop/success.
func (r *Reaper) emitRevokeFailure(ctx context.Context, runID uuid.UUID, errs map[string]string) {
	data, _ := json.Marshal(errs)
	ev := types.AuditEvent{
		ID:        uuid.New(),
		Time:      r.now(),
		RunID:     &runID,
		ActorType: types.ActorSystem,
		Actor:     "wardyn/lifecycle-reaper",
		Action:    "run.revoke",
		Target:    runID.String(),
		Outcome:   "failure",
		Data:      json.RawMessage(data),
	}
	if err := r.recorder.Record(ctx, ev); err != nil {
		r.logger.ErrorContext(ctx, "lifecycle: emit revoke-failure audit event failed",
			"run_id", runID,
			"err", fmt.Sprintf("%v", err),
		)
	}
}
