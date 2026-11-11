// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// reconcileFinalizeTimeout bounds the WHOLE finalize tail (GetRun, CAS, audit
// writes, the revoke cascade, and StopSandbox) so one wedged ContainerStop cannot
// hang the single-threaded sweeper — or the boot pass that gates serveAndShutdown,
// where an unbounded StopSandbox would keep the control plane from ever serving
// /healthz and crashloop the pod. Generous: it must cover a
// graceful SIGTERM→SIGKILL stop (the driver's own stopTimeout is 10s) plus the
// revoke cascade, so 2× that with headroom.
const reconcileFinalizeTimeout = 60 * time.Second

// reconcileFinalize transitions a stranded run to a terminal state — conditional
// on it still being non-terminal so a concurrent kill/complete is never clobbered
// — then runs the revoke cascade and (best-effort) tears the sandbox down.
func (s *Server) reconcileFinalize(ctx context.Context, runID uuid.UUID, to types.RunState, ref, reason string) {
	s.reconcileFinalizeHint(ctx, runID, to, ref, reason, "")
}

// reconcileFinalizeHint is reconcileFinalize for a run finalized from a runner status: a failed run
// records hint, the status's FailureHint, as its failure hint in place of reason, which the audit keeps.
// An empty hint is reconcileFinalize exactly.
func (s *Server) reconcileFinalizeHint(ctx context.Context, runID uuid.UUID, to types.RunState, ref, reason, hint string) {
	// The run's lock is held from the read below through the tail's computer folder pass (lockRunEnd).
	ctx, unlock, locked := s.lockRunEnd(ctx, runID)
	if !locked {
		return
	}
	defer unlock()
	// Bound the whole tail: the sweep loop and the boot pass call
	// this SYNCHRONOUSLY, so an unbounded wedged StopSandbox would stall adoption
	// for the process lifetime (or block boot from serving).
	ctx, cancel := context.WithTimeout(ctx, reconcileFinalizeTimeout)
	defer cancel()
	cur, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: reconcile get run failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	// A run its runner holds is ended by the runner, which this must not race: one with a sandbox there
	// (an interactive run has no execution row: dispatch never Execs its task) and one handed a task (its durable exit
	// receipt). One with neither (a crash between create and dispatch) has nothing on the runner to
	// report, so it is finalized here like any other run that was never dispatched.
	if cur.Placement == types.PlacementLocal && cur.RunnerID != nil && (cur.SandboxRef != "" || s.runnerReceived(ctx, runID)) {
		return
	}
	if isTerminalRunState(cur.State) {
		return // already finalized (e.g. a concurrent kill won)
	}
	if runIsKept(cur) {
		return // stopped on purpose and kept; the lease sweep owns its end
	}
	applied, err := s.casRunState(ctx, runID, cur.State, to)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: reconcile finalize failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if !applied {
		return // someone else won the transition
	}
	if hint == "" {
		hint = reason
	}
	if to == types.RunFailed && hint != "" {
		// Optional-interface, best-effort, in the shape failAndRevoke already
		// uses: reconcileFinalize also finalizes successful terminal states, so
		// the hint write is gated strictly on the FAILED transition — writing it
		// unconditionally would paint a red reason chip on a completed run.
		if setter, ok := s.cfg.Store.(runFailureHintSetter); ok {
			if herr := setter.SetRunFailureHint(ctx, runID, hint); herr != nil {
				slog.WarnContext(ctx, "wardynd: could not persist reconciled run failure hint",
					slog.String("run_id", runID.String()), slog.Any("err", herr))
			}
		}
	}
	// Shared terminal tail (audit run.reconcile → revoke cascade → sandbox
	// teardown with a teardown_error audit on failure → workspace/record
	// settlement). The completion watcher runs the IDENTICAL sequence via
	// finalizeRunTail, so a swallowed StopSandbox error (which would abandon a
	// live/routable container the now-terminal run's next boot skips) can never
	// creep back into just one of the two finalize paths. A scan/verify run that
	// never delivered its result fails fast instead of hanging the workspace; a
	// record run's evidence is captured from whatever audit events landed.
	s.finalizeRunTail(ctx, runID, ref, "run.reconcile", "success",
		map[string]any{"to": string(to), "reason": reason})
}
