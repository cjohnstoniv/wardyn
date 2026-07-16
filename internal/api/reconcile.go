// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log"
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
		if serr != nil || isTerminalRunState(st.State) {
			// The sandbox is gone or already exited: finalize from the exit code
			// (0 => COMPLETED, else FAILED) and run the revoke + teardown cascade.
			final := types.RunFailed
			if serr == nil && st.ExitCode != nil && *st.ExitCode == 0 {
				final = types.RunCompleted
			}
			s.reconcileFinalize(ctx, run.ID, final, run.SandboxRef, "reconciled after restart")
			finalized++
			continue
		}
		// Still alive: re-attach a Status-polling watcher so the run finalizes when
		// the agent exits.
		go s.reconcileWatch(base, run.ID, run.SandboxRef, run.AgentExecID)
		reattached++
	}
	if reattached > 0 || finalized > 0 {
		log.Printf("wardynd: boot reconciliation: %d run(s) re-attached, %d finalized", reattached, finalized)
	}
	return nil
}

// reconcileWatch polls a re-adopted sandbox's Status until it exits, then
// finalizes the run and runs the revoke cascade. Panic-safe (a panic here must
// not crash the control plane).
func (s *Server) reconcileWatch(ctx context.Context, runID uuid.UUID, ref, agentExecID string) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("wardynd: PANIC in reconcile watcher for %s (contained): %v", runID, r)
		}
	}()
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			st, err := s.cfg.Runner.AgentStatus(ctx, ref, agentExecID)
			if err != nil || isTerminalRunState(st.State) {
				final := types.RunFailed
				if err == nil && st.ExitCode != nil && *st.ExitCode == 0 {
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
		log.Printf("wardynd: reconcile get %s: %v", runID, err)
		return
	}
	if isTerminalRunState(cur.State) {
		return // already finalized (e.g. a concurrent kill won)
	}
	applied, err := s.cfg.Store.UpdateRunStateIf(ctx, runID, cur.State, to)
	if err != nil {
		log.Printf("wardynd: reconcile finalize %s: %v", runID, err)
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
