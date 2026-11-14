// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/db"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/google/uuid"
)

// finalizeRunEffects is shared by ordinary finalization and durable runner exit
// receipts, whose success audit already committed with their state transition.
func (s *Server) finalizeRunEffects(ctx context.Context, runID uuid.UUID, ref, action string, data map[string]any, finishAfterStop, gracefulStop bool) error {
	// Each of these runs on a bound of its own (heldCall), not the end hold's deadline: a holder's ctx may have none
	// (the completion watcher, a person's bring-back), and the run's lock is released only when they return. The
	// revoke runs first and is never cut at the hold's deadline; one cut at its own bound stays owed for the revoke
	// queue once its owed mark is written, otherwise the mark is written again in the background until it lands
	// (settleRevokeOwed). The output keeps the bound a kill gives it (finishRunOutputDetached).
	var revokeErr, approvalsErr error
	heldCall(ctx, heldRevokeBound, func(ctx context.Context) { revokeErr = s.revokeRunCascade(ctx, runID) })
	heldCall(ctx, heldStoreCallBound, func(ctx context.Context) { approvalsErr = s.cancelRunApprovals(ctx, runID) })
	errs := []error{revokeErr, approvalsErr}
	if !finishAfterStop {
		heldCall(ctx, killCascadeTimeout, func(ctx context.Context) {
			s.prepareRunOutput(ctx, runID, gracefulStop)
			s.finishRunOutput(ctx, runID)
		})
	}
	// A computer folder's changes come back while the sandbox still holds them (run_computer_folder.go).
	// A pass cut short can keep the sandbox for a resume, its agent stopped and its egress closed:
	// it is then not stopped, its ref stays, and the caller hears errComputerFolderResumes.
	var keep bool
	if fe, forced := ctx.Value(forcedEndKey{}).(forcedEnd); forced {
		keep = fe.keep // a forced end runs no pass: forcedKeepsSandbox decided
	} else {
		keep = s.bringComputerFolderBackAs(ctx, runID, ref, action)
	}
	if keep {
		errs = append(errs, errComputerFolderResumes)
	}
	// Teardown runs on a ctx of its own, which the steps before it cannot use up or cancel
	// (#3466); each write after it on a fresh one, so a stop that overruns its budget takes
	// none of them with it (#3482). Both still end when the run-op lock ctx holds is lost:
	// another replica may then be running this same tail (#3480), and a lock the database no longer
	// holds skips the stop: the runners' StopSandbox does not all honour a cancelled ctx (Docker
	// detaches). The check is made just before the call, so a lock lost during the stop is not seen
	// (Known issues).
	stops := ref != "" && s.cfg.Runner != nil && !keep
	budget := runTeardownBudget
	if stops && s.macOSVMRun(ctx, runID) {
		budget = macOSVMTeardownBudget
	}
	tctx, cancel := teardownCtxWithin(ctx, budget)
	defer cancel()
	teardownOK := false
	lost := stops && lockLost(tctx) // asked once: a lock lost between two asks would skip the stop silently
	if lost {
		// No audit row: another server may own the run now.
		errs = append(errs, fmt.Errorf("the sandbox was not stopped: %w", db.ErrLockLost))
	}
	// The hold's deadline is asked once, on the clock, as the lost lock is, and the skip is said here: tctx's own
	// timer may not have fired yet, and context.Cause(tctx) below would then be nil.
	passed := stops && !lost && holdPassed(tctx)
	if passed {
		// No audit row: the readers (the containment loop and the tail owner) finish the run.
		errs = append(errs, fmt.Errorf("the sandbox was not stopped: %w", errHoldPassed))
	}
	if stops && !lost && !passed {
		if serr := s.cfg.Runner.StopSandbox(tctx, ref); serr != nil {
			errs = append(errs, serr)
			td := make(map[string]any, len(data)+1)
			for k, v := range data {
				td[k] = v
			}
			td["teardown_error"] = serr.Error()
			settleAfterTeardown(ctx, func(sctx context.Context) {
				s.recordAudit(sctx, s.auditEvent(&runID, types.ActorSystem, "wardynd", action,
					ref, "failure", mustJSON(td)))
			})
		} else {
			teardownOK = true
		}
	}
	if finishAfterStop {
		settleAfterTeardown(ctx, func(sctx context.Context) { s.FinishRunOutput(sctx, runID) })
	}
	settleAfterTeardown(ctx, func(sctx context.Context) { s.reconcileWorkspaceRun(sctx, runID) })
	settleAfterTeardown(ctx, func(sctx context.Context) { s.reconcileRecordRun(sctx, runID) })
	// Clear the ref LAST, and only when this call's own StopSandbox succeeded.
	// LAST because reconcileRecordRun above still reads run.SandboxRef to tell
	// "the sandbox never started" from "capture was empty". Success-only because
	// the boot orphan sweep keys on a terminal run STILL carrying a ref — after
	// this line that means exactly "teardown failed", the case it exists for.
	// The kill tail clears the ref after a confirmed kill (killTeardownTail); the
	// idle stop tears down on its own path and does not, and the evidence
	// endpoints' terminal-state guard covers those runs. A failed clear is
	// deliberately ignored: it degrades to the pre-clear behavior the boot
	// sweep already retries.
	if teardownOK && s.cfg.Store != nil {
		settleAfterTeardown(ctx, func(sctx context.Context) { errs = append(errs, s.clearSandboxRef(sctx, runID)) })
	}
	return errors.Join(append(errs, cmp.Or(ctx.Err(), context.Cause(tctx)))...)
}

// forcedEndKey carries, on the ctx of a forced end's tail (forceKeptStop), the sandbox decision made without the
// run's lock. The tail then runs no pass back: the run's lock is held by another operation, which may be running
// one. keep is forcedKeepsSandbox's answer. The tail owner's give-up (tailFinalize) carries keep false: its record
// is final, so no pass is owed and the sandbox is stopped.
type forcedEndKey struct{}

type forcedEnd struct{ keep bool }

// errComputerFolderResumes is finalizeRunEffects keeping a sandbox for its computer folder's pass
// back to resume (bringComputerFolderBack).
var errComputerFolderResumes = errors.New("the sandbox is kept: its computer folder's sync back resumes on a later sweep")

// runTeardownBudget is the time a run's teardown keeps for itself: StopSandbox, whose graceful
// stop alone is 10 s on Docker (the driver's stopTimeout), and the audit and status writes after
// it. The folder pass before teardown ends this long before the caller's deadline
// (bringComputerFolderBack).
const runTeardownBudget = 15 * time.Second

// macOSVMTeardownBudget is runTeardownBudget for a macOS VM run. Its runner's stop asks the guest to
// shut down and waits up to 30 s for it, then for the helper to exit, and bounds that destroy at 80 s:
// held to a runner exit's 30 s, every such stop overran and was audited as a failed teardown (#3886).
const macOSVMTeardownBudget = 90 * time.Second

// macOSVMRun reports whether runID is a macOS VM run, read on a teardownCtx. A run it cannot read is not.
func (s *Server) macOSVMRun(ctx context.Context, runID uuid.UUID) bool {
	if s.cfg.Store == nil {
		return false
	}
	rctx, cancel := teardownCtx(ctx)
	defer cancel()
	run, err := s.cfg.Store.GetRun(rctx, runID)
	return err == nil && run.MachineKind.IsMacOSVM()
}

// teardownCtx is ctx's values with none of its cancellation but a lost lock's
// (db.WithoutCancelUnlessLost), on a deadline of its own: ctx's deadline or
// runTeardownBudget from now, whichever is later. A ctx with no deadline gets
// reconcileFinalizeTimeout, the bound the sweeps give the whole finalize tail.
func teardownCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return teardownCtxWithin(ctx, runTeardownBudget)
}

// teardownCtxWithin is teardownCtx with floor in place of runTeardownBudget, and in place of
// reconcileFinalizeTimeout when floor is the longer. Under an end hold (holdDeadline) it ends at the hold's
// deadline when that comes first, with errHoldPassed as its cause, floor or not: a deadline already passed
// gives a ctx that is done.
func teardownCtxWithin(ctx context.Context, floor time.Duration) (context.Context, context.CancelFunc) {
	d := max(reconcileFinalizeTimeout, floor)
	if dl, ok := ctx.Deadline(); ok {
		d = max(time.Until(dl), floor)
	}
	fenced, release := db.WithoutCancelUnlessLost(ctx)
	tctx, cancel := withinHold(fenced, d)
	return tctx, func() { cancel(); release() }
}

// settleAfterTeardown runs one write after a run's teardown on a fresh runTeardownBudget of
// its own, with teardownCtx's fencing and none of the stop's deadline, ending at an end hold's
// deadline as teardownCtxWithin does.
func settleAfterTeardown(ctx context.Context, write func(context.Context)) {
	fenced, release := db.WithoutCancelUnlessLost(ctx)
	defer release()
	sctx, cancel := withinHold(fenced, runTeardownBudget)
	defer cancel()
	write(sctx)
}

// withinHold is context.WithTimeout(ctx, d), ended instead at the deadline of the end hold ctx is under,
// with errHoldPassed as its cause, when that comes first.
func withinHold(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if dl, ok := holdDeadline(ctx); ok && time.Until(dl) < d {
		return context.WithDeadlineCause(ctx, dl, errHoldPassed)
	}
	return context.WithTimeout(ctx, d)
}

// clearSandboxRef clears a terminal run's ref. On Postgres a runner run's
// binding is snapshotted first, in the same transaction, so its retirement
// can still bind the runner's kill and retire_run results to it.
func (s *Server) clearSandboxRef(ctx context.Context, runID uuid.UUID) error {
	if st, ok := s.cfg.Store.(store.PG); ok {
		return st.ClearSandboxRefRetaining(ctx, runID)
	}
	return s.cfg.Store.SetSandboxRef(ctx, runID, "")
}

// recordRunnerOutputWatermark records, before a runner run's create, whether
// the create asks the runner to keep its output. A failed write leaves the run
// with no row, which only means it is never retired (its runner keeps its
// output and spool, as before retirement existed).
func (s *Server) recordRunnerOutputWatermark(ctx context.Context, run types.AgentRun, capture bool) {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || run.RunnerID == nil {
		return
	}
	if err := st.InsertRunnerOutputWatermark(ctx, run.ID, capture); err != nil {
		slog.WarnContext(ctx, "wardynd: runner run output watermark not recorded; the run will not be retired",
			slog.String("run_id", run.ID.String()), slog.Any("err", err))
	}
}

// produceRunnerRetirements is the run watcher's retirement pass: it abandons
// the open retirements of revoked runners, then takes one step of each
// eligible terminal runner run's retirement (store.AdvanceRunnerRetirement).
// The runner receives what it queues at its next fresh session.
func (s *Server) produceRunnerRetirements(ctx context.Context) error {
	st, ok := s.cfg.Store.(store.PG)
	if !ok || s.cfg.RunnerAuditPrepare == nil {
		return nil
	}
	if _, err := st.AbandonRevokedRunnerRetirements(ctx, s.cfg.RunnerAuditPrepare); err != nil {
		return err
	}
	candidates, err := st.RunnerRetirementCandidates(ctx)
	if err != nil {
		return err
	}
	var errs []error
	for _, c := range candidates {
		// An offline runner answers nothing; its runs wait for it to connect.
		if !s.runnerEndpoint(c.RunnerID).link.Online() {
			continue
		}
		if _, err := st.AdvanceRunnerRetirement(ctx, c.RunID, s.cfg.RunnerAuditPrepare); err != nil && !errors.Is(err, store.ErrNotFound) {
			errs = append(errs, fmt.Errorf("run %s: %w", c.RunID, err))
		}
	}
	return errors.Join(errs...)
}

// isTerminalRunState reports whether a run is in a terminal (already-ended)
// state. The kill-switch must not re-kill or clobber a run that already ended
// (COMPLETED/FAILED by the watcher, or KILLED/STOPPED/ARCHIVED earlier). The set
// itself lives on types.RunState so the CLI and SDK share one definition.
func isTerminalRunState(st types.RunState) bool { return st.IsTerminal() }
