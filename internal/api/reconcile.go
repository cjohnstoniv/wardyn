// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ReconcileOnBoot re-derives the state of every run left non-terminal by a
// PREVIOUS wardynd process (a crash, OOM, deploy, or Ctrl-C) so it is not
// stranded RUNNING forever with a live sandbox and un-revoked credentials (C3).
// The per-run completion watcher is an in-process goroutine that does not survive
// a restart; this rebuilds the safety net at boot. Best-effort: errors are logged,
// never fatal. Re-attached watchers run on the daemon base context so they outlive
// this call. AgentStatus observes the AGENT (via the persisted agent_exec_id), not
// just the container, so an idle-container exec run whose agent already exited is
// finalized instead of stranded: the previous process's in-memory exec map is gone,
// but the exec id survives on the run row (U008/U039).
func (s *Server) ReconcileOnBoot(ctx context.Context) error {
	if s.cfg.Runner == nil {
		return nil
	}
	runs, err := s.cfg.Store.ListRuns(ctx)
	if err != nil {
		return err
	}
	base := s.cfg.BaseCtx
	if base == nil {
		base = context.Background()
	}
	var reattached, finalized int
	for _, run := range runs {
		if isTerminalRunState(run.State) {
			continue
		}
		// A non-terminal run that never got a sandbox can never be dispatched after
		// a restart — finalize it FAILED and revoke.
		if run.SandboxRef == "" {
			s.reconcileFinalize(ctx, run.ID, types.RunFailed, "", "no sandbox after restart")
			finalized++
			continue
		}
		st, serr := s.cfg.Runner.AgentStatus(ctx, run.SandboxRef, run.AgentExecID)
		// A genuinely-gone sandbox/agent reports a terminal STATE (RunStopped), not
		// an error — an error means the probe couldn't determine liveness (a docker
		// daemon blip at boot). Do NOT finalize a possibly-healthy run on a transient
		// error; re-attach a watcher that retries and only gives up after a bounded
		// error run. Only a definitive terminal STATE finalizes here.
		if serr == nil && isTerminalRunState(st.State) {
			final := types.RunFailed
			if st.ExitCode != nil && *st.ExitCode == 0 {
				final = types.RunCompleted
			}
			s.reconcileFinalize(ctx, run.ID, final, run.SandboxRef, "reconciled after restart")
			finalized++
			continue
		}
		// Still alive (or momentarily unreachable): re-attach a watcher so the run
		// finalizes when the agent actually exits, not on a transient probe error.
		go s.reconcileWatch(base, run.ID, run.SandboxRef, run.AgentExecID)
		reattached++
	}
	if reattached > 0 || finalized > 0 {
		slog.InfoContext(ctx, "wardynd: boot reconciliation",
			slog.Int("reattached", reattached), slog.Int("finalized", finalized))
	}
	return nil
}

// reconcileMaxProbeErrors bounds how many CONSECUTIVE AgentStatus probe errors a
// reconcile watcher tolerates before giving up on a persistently-unreachable
// sandbox. A transient error (a docker daemon blip) must NOT finalize a healthy
// RUNNING run — but a permanently-broken ref must not poll forever either. ~1 min
// at the 5s tick.
const reconcileMaxProbeErrors = 12

// reconcileWatch polls a re-adopted sandbox's agent liveness until it exits, then
// finalizes the run and runs the revoke cascade. Panic-safe (a panic here must
// not crash the control plane).
func (s *Server) reconcileWatch(ctx context.Context, runID uuid.UUID, ref, agentExecID string) {
	defer func() {
		if r := recover(); r != nil {
			slog.ErrorContext(ctx, "wardynd: PANIC in reconcile watcher (contained)",
				slog.String("run_id", runID.String()), slog.Any("panic", r))
		}
	}()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	errs := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st, err := s.cfg.Runner.AgentStatus(ctx, ref, agentExecID)
			if err != nil {
				// A transient probe error is NOT "the run finished" — finalizing here
				// would false-kill a healthy RUNNING run on a docker daemon blip.
				// Tolerate a bounded run of consecutive errors, then give up.
				errs++
				if errs < reconcileMaxProbeErrors {
					continue
				}
				s.reconcileFinalize(ctx, runID, types.RunFailed, ref, "reconciled: sandbox persistently unreachable")
				return
			}
			errs = 0
			if isTerminalRunState(st.State) {
				final := types.RunFailed
				if st.ExitCode != nil && *st.ExitCode == 0 {
					final = types.RunCompleted
				}
				s.reconcileFinalize(ctx, runID, final, ref, "reconciled exit")
				return
			}
		}
	}
}

// reconcileFinalize transitions a stranded run to a terminal state — conditional
// on it still being non-terminal so a concurrent kill/complete is never clobbered
// — then runs the revoke cascade and (best-effort) tears the sandbox down.
func (s *Server) reconcileFinalize(ctx context.Context, runID uuid.UUID, to types.RunState, ref, reason string) {
	cur, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: reconcile get run failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if isTerminalRunState(cur.State) {
		return // already finalized (e.g. a concurrent kill won)
	}
	applied, err := s.cfg.Store.UpdateRunStateIf(ctx, runID, cur.State, to)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: reconcile finalize failed",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return
	}
	if !applied {
		return // someone else won the transition
	}
	s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.reconcile",
		runID.String(), "success", mustJSON(map[string]any{"to": string(to), "reason": reason})))
	s.revokeRunCascade(ctx, runID)
	// Tear the sandbox down and RECORD a failed teardown. A swallowed StopSandbox
	// error leaves a live/routable container that the next boot skips (the run is
	// now terminal ⇒ not reconciled), abandoning it forever with no record. Mirror
	// the completion watcher's teardown_error audit (runs.go) so the abandoned
	// sandbox is visible in the system of record instead of vanishing silently.
	if ref != "" && s.cfg.Runner != nil {
		if serr := s.cfg.Runner.StopSandbox(ctx, ref); serr != nil {
			s.recordAudit(ctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", "run.reconcile",
				ref, "failure", mustJSON(map[string]any{
					"to": string(to), "reason": reason, "teardown_error": serr.Error(),
				})))
		}
	}
	// Settle any workspace this stranded run was an import step for: a scan/
	// verify run that never delivered its result fails fast instead of hanging
	// the workspace across the restart, and a record run's evidence is captured
	// from whatever audit events landed before the crash. Both are idempotent.
	s.reconcileWorkspaceRun(ctx, runID)
	s.reconcileRecordRun(ctx, runID)
}
