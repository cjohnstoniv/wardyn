// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// The terminal-sandbox sweep primitive: SweepTerminalSandboxes (unbounded,
// the operator route) and SweepTerminalSandboxesPage (bounded, the periodic
// ticker — cmd/wardynd's runTerminalSandboxSweeper, #710), sharing one body.
// Split from runs_lifecycle.go along that seam to keep it under the file-size
// cap.

package api

import (
	"context"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// SweepTerminalSandboxes is the retry surface for two gaps in the same
// family: a failed StopSandbox/RevokeRun step inside a prior finalize
// (finalizeRunTail) or kill (handleKillRun) leaves a terminal run row with a
// sandbox nothing else revisits — ReconcileOnBoot skips terminal runs outright
// (reconcile.go), and handleKillRun 409s a non-KILLED terminal run rather than
// risk corrupting its recorded outcome — and, separately, killTeardownTail
// itself can be interrupted (a shutdown past its grace, a crash) after the
// KILLED CAS has already landed but before the teardown/revoke/audit tail
// finishes, leaving a run correctly marked KILLED with no run.kill row at all
// (see recoverAbandonedKillTail).
//
// This PROBES the runner for every terminal run with a SandboxRef (never
// trusts the row's own state — the row is terminal by definition, so only a
// live probe can tell orphaned from settled), tears down + re-runs the
// idempotent revoke cascade for anything still reported running, retries any
// KILLED row whose own kill tail looks abandoned, and reports how many it
// swept. Read-only on run STATE: it never transitions a run (a terminal row's
// recorded outcome is untouched either way), only the runner + broker/
// identity + audit side effects finalizeRunTail/killTeardownTail already
// perform for every other terminal transition.
//
// Caller: the operator route (POST /admin/sandboxes/sweep, unbounded — an
// operator asked for it once) and, bounded, the periodic ticker
// (cmd/wardynd's runTerminalSandboxSweeper, via SweepTerminalSandboxesPage
// below).
func (s *Server) SweepTerminalSandboxes(ctx context.Context) (int, error) {
	runs, err := s.cfg.Store.ListRuns(ctx)
	if err != nil {
		return 0, err
	}
	swept, _ := s.sweepTerminalSandboxRuns(ctx, runs)
	return swept, nil
}

// SweepTerminalSandboxesPage is SweepTerminalSandboxes bounded to one page of
// runs, for the periodic ticker: unlike the operator-triggered
// SweepTerminalSandboxes, which is deliberately unbounded because a human
// asked for it once, a ticker's cost must never grow with run history. It
// pages through store.Pager.ListRunsPage (the same ordering ListRuns'
// unbounded read already uses, created_at DESC) — the caller advances
// page.Offset by pageLen each tick and wraps back to 0 once pageLen comes
// back short of page.Limit, so repeated ticks eventually cover the whole
// table at bounded per-tick cost. Falls back to sweeping every run in one
// pass when the store does not implement store.Pager (a fake without it, in
// tests) — real PG always does.
func (s *Server) SweepTerminalSandboxesPage(ctx context.Context, page store.Page) (swept, pageLen int, err error) {
	pager, ok := s.cfg.Store.(store.Pager)
	if !ok {
		runs, lerr := s.cfg.Store.ListRuns(ctx)
		if lerr != nil {
			return 0, 0, lerr
		}
		swept, _ = s.sweepTerminalSandboxRuns(ctx, runs)
		return swept, len(runs), nil
	}
	runs, lerr := pager.ListRunsPage(ctx, page)
	if lerr != nil {
		return 0, 0, lerr
	}
	swept, _ = s.sweepTerminalSandboxRuns(ctx, runs)
	return swept, len(runs), nil
}

// sweepTerminalSandboxRuns is SweepTerminalSandboxes and
// SweepTerminalSandboxesPage's shared body: for every TERMINAL run in runs
// (never a page's non-terminal rows — a ticker's page can include live runs,
// which this simply skips, exactly as the unbounded sweep always has),
// probe/recover it. Returns how many it swept and how many terminal rows it
// actually examined (informational only; page length is what callers page on).
func (s *Server) sweepTerminalSandboxRuns(ctx context.Context, runs []types.AgentRun) (swept, scanned int) {
	if s.cfg.Runner == nil {
		return 0, 0
	}
	for _, run := range runs {
		if !isTerminalRunState(run.State) {
			continue
		}
		scanned++
		if s.sweepTerminalSandboxRun(ctx, run) {
			swept++
		}
	}
	return swept, scanned
}

// sweepTerminalSandboxRun probes/recovers one terminal run and reports whether
// it swept it. It holds the run's lock, so only one sweep at a time works on a
// run, across replicas: the operator route and the ticker (or two replicas'
// tickers) would otherwise both tear the same run down and double its run.kill
// rows. A run whose lock is held is skipped; a later pass decides.
func (s *Server) sweepTerminalSandboxRun(ctx context.Context, run types.AgentRun) bool {
	ctx, unlock, locked := s.tryLockRunOp(ctx, run.ID)
	if !locked {
		return false
	}
	defer unlock()
	// KILLED rows get an extra, ref-independent check first: a tail that
	// died before ever reaching KillSandbox leaves SandboxRef exactly as it
	// was (possibly already empty), so the ref-probe below would never see
	// it. A run this recovers still falls through to the ref-probe below on
	// the NEXT pass only if it needs to — this pass counts it once.
	if run.State == types.RunKilled && s.recoverAbandonedKillTail(ctx, run) {
		return true
	}
	if run.SandboxRef == "" {
		return false
	}
	st, serr := s.cfg.Runner.Status(ctx, run.SandboxRef)
	if serr != nil || st.State != types.RunRunning {
		return false // already gone (or unprobeable) — the normal, settled case
	}
	s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "sandbox.sweep")
	s.revokeRunCascade(ctx, run.ID)
	return true
}

// killTailRecoveryGrace is how long a KILLED run's own kill tail
// (killTeardownTail) is given to finish before the sweep will consider it
// abandoned. Every caller of killTeardownTail bounds it at killCascadeTimeout —
// handleKillRun's synchronous cascade and supersedeOneLoginRun's detached
// goroutine each wrap their own context.WithTimeout(killCascadeTimeout) around
// it — so a KILLED row whose transition (run.UpdatedAt, stamped by the winning
// CAS the moment the tail STARTS) is older than that bound, and still has no
// successful run.kill row, cannot still have a live tail running; the margin
// on top absorbs scheduling/clock slack rather than racing the bound exactly.
const killTailRecoveryGrace = killCascadeTimeout + 30*time.Second

// killTailRecoveryTimeout bounds one recovered kill tail: the killCascadeTimeout
// the normal kill path gives it. A var only so a test can shrink it.
var killTailRecoveryTimeout = killCascadeTimeout

// killTailSettled lists the audit rows that each prove a KILLED run needs no
// kill-tail recovery. Matched with an EXISTS over the run's whole trail, never
// a bounded read: a settled run.kill behind more rows than the window was
// missed, and the run was re-killed on every sweep.
var killTailSettled = []store.AuditFilter{
	// Its own kill tail (killTeardownTail) finished cleanly.
	{Action: "run.kill", Outcome: "success"},
	// reclaimProbeRun (site_config_probe.go) KILLs a hung probe through
	// finalizeRunTail, which writes this row and never run.kill. Any outcome:
	// that path records the reclaim itself as a failure by design.
	{Action: "site_config.probe.kill"},
}

// recoverAbandonedKillTail retries an interrupted kill tail: a KILLED run
// whose killTeardownTail died — a shutdown past backgroundShutdownBudget, a
// crash, a killed process — before writing its run.kill audit row, leaving the
// run correctly marked KILLED but with no record the teardown/revocation
// cascade ever ran, its sandbox possibly still live, and its credentials
// possibly still un-revoked. Unlike the SandboxRef probe in
// SweepTerminalSandboxes, this also covers the NO-REF case: a tail that died
// before ever calling KillSandbox leaves SandboxRef exactly as claimKillTransition
// found it, with nothing else in the row to probe — the missing run.kill row
// is the only signal.
//
// Eligibility is read fresh from the run's own state and audit trail every
// call, never from a flag this sweep sets, so recovery is naturally
// at-least-once: a sweep interrupted mid-recovery, or whose own run.kill write
// itself fails to persist, is retried by the very next pass, because the
// successful row it looks for still will not exist. It reports whether it
// attempted a recovery (regardless of that attempt's own outcome), so the
// caller can count it swept.
//
// killTailRecoveryGrace guards against racing a tail that is simply still
// running: entering killTeardownTail a second time WHILE the first is still
// mid-flight would double the run.kill audit row and double-drive the runner
// concurrently. A KILLED row younger than the grace is left alone — it may
// still have a live tail finishing on its own.
func (s *Server) recoverAbandonedKillTail(ctx context.Context, run types.AgentRun) bool {
	if s.cfg.Now().UTC().Sub(run.UpdatedAt) < killTailRecoveryGrace {
		return false // may still be an in-flight tail — leave it alone
	}
	matcher, ok := s.cfg.Store.(store.RunAuditMatcher)
	if !ok {
		return false // cannot answer exactly: never re-kill on a guess
	}
	for _, f := range killTailSettled {
		found, err := matcher.HasRunAuditEvent(ctx, run.ID, f)
		if err != nil || found {
			return false // settled, or unprobeable this pass (the next pass tries again)
		}
	}
	// Bounded like every other killTeardownTail caller, so a hung runner costs
	// the operator's request at most one budget per run. A tail cut short
	// audits run.kill as a failure and is retried by the next pass.
	tailCtx, cancel := context.WithTimeout(ctx, killTailRecoveryTimeout)
	defer cancel()
	extra := map[string]any{"reason": "sweep_recovered_kill_tail"}
	if s.runnerProvenRevoked(tailCtx, run) {
		// No teardown goes to a revoked runner (revokedRunKillTail); the rest of the tail still runs.
		run.SandboxRef = ""
		extra["sandbox_stop"] = string(placement.ReasonRunnerRevoked)
	}
	// queuedOK: nobody retries a recovery by hand, and a stop the offline runner's
	// queue already holds is applied when the runner returns (killRunAtLifetime).
	s.killTeardownTail(tailCtx, run, types.ActorSystem, "wardynd", extra, true)
	return true
}

// runnerProvenRevoked reports whether the runner run is bound to is durably
// revoked: its own row, read now, in the revoked state. Everything short of
// that is false — a run on no runner, a store without runners, a failed read.
// A runner that is offline or has no route on this replica proves nothing.
func (s *Server) runnerProvenRevoked(ctx context.Context, run types.AgentRun) bool {
	rs, ok := s.cfg.Store.(store.RunnerStore)
	if !ok || run.RunnerID == nil {
		return false
	}
	row, err := rs.GetRunner(ctx, *run.RunnerID)
	return err == nil && row.State == types.RunnerRevoked
}
