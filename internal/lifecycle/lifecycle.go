// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package lifecycle implements workspace lifecycle automation: the Reaper loop
// finds RUNNING agent runs that have been idle past their policy's
// AutoStopAfterSec threshold and stops them, emitting a "run.autostop" audit
// event for each.
//
// # Idleness definition (v0) — KNOWN LIMITATION
//
// "Idleness" here is NOT activity-based: it is wall-clock age of
// agent_runs.updated_at. The clock resets only when something writes that row
// through the store — state transitions, sandbox_ref updates, or an explicit
// store.TouchRun keepalive (which the interactive-attach handler calls). It does
// NOT reset on genuine in-sandbox agent activity (CPU, proxied egress, file
// writes) that never touches the store row. Consequently a run that is busy but
// whose store row is otherwise unchanged will be stopped once updated_at ages
// past its policy threshold.
//
// This is a deliberate v0 simplification. The seam to do better already exists:
// store.TouchRun bumps updated_at without other side effects, so once the
// wardyn-proxy bumps updated_at on every proxied request (or the runner reports
// liveness), genuine activity will keep the workspace alive automatically — no
// change to this package is required. Until then, operators who need an
// unbounded session should use the never-reap escape hatch (policy
// AutoStopAfterSec < 0). This residual risk is documented here and should be
// mirrored in threatmodel/ if appropriate.
//
// # AutoStopAfterSec semantics (policy auto_stop_after_sec)
//
//	> 0  idle timeout in seconds (stop after this much wall-clock idleness)
//	  0  DISABLED — the run is never reaped (matches docs/policies; this is the
//	     default for a run with no auto-stop configured, since the store COALESCEs
//	     an absent policy field to 0)
//	< 0  never reaped (explicit interactive escape hatch; equivalent to 0 for the
//	     reaper, but kept distinct so an operator can express intent loudly)
package lifecycle

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

const (
	// defaultInterval is how often the reaper wakes to scan for idle runs.
	defaultInterval = time.Minute

	// defaultStopTimeout bounds a single StopRun call so one hung StopSandbox
	// cannot stall the whole reap loop (finding #3). Each idle run gets its own
	// deadline derived from the tick's context; on timeout the stop is logged
	// and skipped, and the run is retried on the next tick.
	defaultStopTimeout = 2 * time.Minute
)

// RunSummary is the minimal projection a Store must return for idle detection.
// It carries everything the Reaper needs without leaking the full run row.
type RunSummary struct {
	ID        uuid.UUID
	UpdatedAt time.Time
	// PolicyAutoStopAfterSec is the value from the run's attached policy spec.
	// Semantics (see package doc): 0 means DISABLED (never reap), a NEGATIVE value
	// also means "never reap", and a POSITIVE value is the idle timeout in seconds.
	// The Store must JOIN to the policy table and surface this field; a zero value
	// is intentional (a run with no auto-stop configured is never reaped, not a
	// missing join).
	PolicyAutoStopAfterSec int
}

// Store is the narrow persistence interface the Reaper requires.
//
// The real adapter is trivial: store.ListRunningWithPolicy wraps the pgxpool
// and maps from (agent_runs JOIN run_policies). Tests supply a fake.
//
// Method shapes deliberately mirror the existing store package naming
// conventions (List*, returning a slice) so the integrator's adapter
// is mechanical.
type Store interface {
	// ListRunningWithPolicy returns all runs currently in state RUNNING
	// together with the auto_stop_after_sec value from their attached policy
	// (0 when no policy is attached or the policy field is unset).
	ListRunningWithPolicy(ctx context.Context) ([]RunSummary, error)
}

// StopOutcome is what StopRun reports back to the Reaper beyond the raw error.
type StopOutcome struct {
	// Applied reports whether the stop actually transitioned the run from RUNNING
	// to STOPPED. It is false when a concurrent kill/complete had already moved the
	// run terminal, OR when the idleness guard no-op'd because the run's updated_at
	// advanced past the reaper's snapshot (an active `wardyn attach` touched it
	// after the scan — finding N3). Either way the reaper must NOT emit a spurious
	// run.autostop (finding #1).
	Applied bool
	// Errors, when non-empty, carries the teardown/revocation failures that
	// occurred AFTER the stop transition won (Applied=true) but which left the run
	// not fully contained: "identity_error" / "broker_error" (the run token or
	// minted broker creds may still be live — finding N1), and "teardown_error"
	// (the sandbox may still be routable). The reaper emits these as a distinct
	// run.revoke/failure audit event so the live-credential/live-sandbox window is
	// visible, mirroring handleKillRun/revokeRunCascade. Nil on a clean stop.
	Errors map[string]string
}

// Stopper stops a single run. The implementation is expected to conditionally
// transition the run's state to STOPPED (RUNNING->STOPPED only, guarded on the
// snapshot's idleness) and then call runner.StopSandbox + the revoke cascade;
// all concerns are hidden behind this interface so the lifecycle package stays
// target-agnostic.
type Stopper interface {
	// StopRun gracefully stops the run identified by runID. Implementations must
	// be idempotent: stopping an already-stopped run returns ({Applied:false}, nil).
	//
	// notAfter is the run's updated_at from the reaper's tick snapshot: the stop
	// transition must be conditional on updated_at not having advanced past it, so
	// a run an active attach touched after the snapshot is NOT stopped (finding N3).
	//
	// The returned StopOutcome reports whether the transition applied and any
	// post-transition teardown/revocation failures. A non-nil error means the stop
	// failed outright; the outcome is meaningless and the reaper logs and skips.
	StopRun(ctx context.Context, runID uuid.UUID, notAfter time.Time) (StopOutcome, error)
}

// Recorder is the minimal audit interface the Reaper needs. It matches
// audit.Recorder exactly so the integrator can pass a store.Recorder directly.
type Recorder interface {
	Record(ctx context.Context, ev types.AuditEvent) error
}

// Config holds optional overrides for Reaper behaviour. Zero value is valid:
// defaults are applied in New.
type Config struct {
	// Interval is how often the reaper scans. Default: 1 minute.
	Interval time.Duration
	// Now overrides the wall clock. Nil means use real time.
	// (overridable in tests, mirrors embedded.Provider's now func idiom)
	Now func() time.Time
}

// Reaper is the idle-workspace garbage collector. It runs a periodic loop
// that finds RUNNING workspaces idle past their policy threshold and stops
// them.
//
// Reaper.Run is designed to be called in a dedicated goroutine and blocks
// until ctx is cancelled. A stopper error is logged and skipped so that one
// broken sandbox never prevents the rest from being reaped on the same tick.
type Reaper struct {
	store    Store
	stopper  Stopper
	recorder Recorder
	now      func() time.Time
	interval time.Duration
	logger   *slog.Logger
}

// New constructs a Reaper. store, stopper, and recorder are required and must
// be non-nil. cfg may be zero-valued.
func New(store Store, stopper Stopper, recorder Recorder, cfg Config) *Reaper {
	r := &Reaper{
		store:    store,
		stopper:  stopper,
		recorder: recorder,
		now:      cfg.Now,
		interval: cfg.Interval,
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

// Run starts the reap loop. It returns when ctx is cancelled (typically at
// process shutdown). The first tick fires after one full Interval so that the
// control plane can finish starting up before the first scan.
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

// Tick is one reap scan. It is exported so that integration callers and tests
// can drive it directly; in production code always use Run instead.
func (r *Reaper) Tick(ctx context.Context) {
	runs, err := r.store.ListRunningWithPolicy(ctx)
	if err != nil {
		r.logger.ErrorContext(ctx, "lifecycle: list running runs failed", "err", err)
		return
	}

	now := r.now()

	for _, run := range runs {
		// Auto-stop disabled / never-reap opt-out (finding #2): a policy
		// AutoStopAfterSec <= 0 means the run is exempt from idle auto-stop
		// entirely — the reaper must never stop it regardless of how long it has
		// been idle. 0 is DISABLED (the default for a run with no auto-stop
		// configured); a negative value is the explicit interactive escape hatch
		// for unbounded attach sessions. Either way: skip.
		if run.PolicyAutoStopAfterSec <= 0 {
			continue
		}
		threshold := r.thresholdFor(run)
		idleFor := now.Sub(run.UpdatedAt)
		if idleFor < threshold {
			continue
		}

		// Run is idle past its threshold: stop it. Bound each stop with its own
		// deadline (finding #3) so a single hung StopSandbox cannot stall the
		// whole reap loop; the run is simply retried on the next tick.
		runID := run.ID
		stopCtx, cancel := context.WithTimeout(ctx, defaultStopTimeout)
		// Pass the snapshot's updated_at so the stop transition is conditional on
		// the run not having been touched (e.g. by an active attach keepalive)
		// since the scan — finding N3.
		out, err := r.stopper.StopRun(stopCtx, runID, run.UpdatedAt)
		cancel()
		if err != nil {
			// Log and continue — one broken sandbox must not kill the loop.
			r.logger.ErrorContext(ctx, "lifecycle: stop run failed",
				"run_id", runID,
				"idle_for", idleFor,
				"threshold", threshold,
				"err", err,
			)
			continue
		}
		if !out.Applied {
			// A concurrent kill/complete already moved the run terminal, OR the run
			// was touched after the snapshot (active attach) so the idle-guarded
			// transition was a no-op. Either way do NOT emit a spurious run.autostop
			// (findings #1 / N3) — the run was not autostopped.
			r.logger.DebugContext(ctx, "lifecycle: stop was a no-op (already terminal or touched after snapshot)",
				"run_id", runID,
			)
			continue
		}

		r.emitAutoStop(ctx, runID, idleFor, threshold)

		// The stop transition won but a teardown/revocation step failed: the run is
		// STOPPED yet not fully contained (live token/broker creds or a live
		// sandbox). Emit a DISTINCT run.revoke/failure event so that window is
		// visible in the audit log — the run.autostop above alone would dishonestly
		// read as a fully-successful stop (finding N1). Mirrors handleKillRun.
		if len(out.Errors) > 0 {
			r.emitRevokeFailure(ctx, runID, out.Errors)
		}
	}
}

// thresholdFor returns the idle threshold for a run. The run's policy
// AutoStopAfterSec (> 0) is the timeout in seconds. This preserves the invariant
// that workspace-local configuration can only NARROW policy, never widen it —
// the per-run override is sourced from the policy, not from workspace config.
//
// Tick filters out every run with AutoStopAfterSec <= 0 (DISABLED / never-reap)
// BEFORE calling this, so thresholdFor is only ever invoked with a positive
// value.
func (r *Reaper) thresholdFor(run RunSummary) time.Duration {
	return time.Duration(run.PolicyAutoStopAfterSec) * time.Second
}

// emitAutoStop writes a "run.autostop" audit event. Failures are logged and
// swallowed: audit must never gate the stop path. The append-only audit
// invariant is upheld by the store layer (Postgres trigger).
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

// emitRevokeFailure writes a "run.revoke" / failure audit event carrying the
// per-target teardown/revocation errors ("identity_error" / "broker_error" /
// "teardown_error") from a stop that transitioned the run to STOPPED but did not
// fully contain it. This mirrors handleKillRun/revokeRunCascade's distinct
// failure event so a silently-failed idle-stop revoke — which leaves the run
// token valid until its <=1h TTL, or the sandbox routable — is visible in the
// system of record instead of hiding behind run.autostop/success (finding N1).
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
