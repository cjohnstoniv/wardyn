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
		// KILLED rows get an extra, ref-independent check first: a tail that
		// died before ever reaching KillSandbox leaves SandboxRef exactly as it
		// was (possibly already empty), so the ref-probe below would never see
		// it. A run this recovers still falls through to the ref-probe below on
		// the NEXT pass only if it needs to — this pass counts it once.
		if run.State == types.RunKilled && s.recoverAbandonedKillTail(ctx, run) {
			swept++
			continue
		}
		if run.SandboxRef == "" {
			continue
		}
		st, serr := s.cfg.Runner.Status(ctx, run.SandboxRef)
		if serr != nil || st.State != types.RunRunning {
			continue // already gone (or unprobeable) — the normal, settled case
		}
		s.stopSandboxOrAudit(ctx, run.ID, run.SandboxRef, "sandbox.sweep")
		s.revokeRunCascade(ctx, run.ID)
		swept++
	}
	return swept, scanned
}
