// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/api"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// terminalSandboxSweepInterval is how often the ticker sweeps one page of
// terminal runs for an abandoned sandbox or kill tail (#710). Not
// configurable: like runSecretSweeper/runCredentialSweeper (fixed
// housekeeping cadences, no operator-facing urgency), this has no flag —
// only the sweepers an operator might reasonably want to tune for their own
// deployment (the lifecycle reaper's autostop interval, approval expiry,
// recording retention) take one.
const terminalSandboxSweepInterval = 5 * time.Minute

// terminalSandboxSweepPageSize bounds each tick's cost: SweepTerminalSandboxesPage
// reads at most this many runs (newest-first) per tick, so the ticker's cost
// never grows with run history the way the operator-triggered, deliberately
// unbounded SweepTerminalSandboxes does. The run rows this exists to catch (an
// abandoned kill tail, a leaked sandbox after a crash) are rare, so being slow
// to REACH one costs nothing a human would notice — a deployment with more
// terminal runs than this simply takes more ticks to wrap the whole table.
const terminalSandboxSweepPageSize = 200

// terminalSandboxSweepTickTimeout bounds one locked tick end-to-end — lock
// acquisition through the sweep's own per-run calls — mirroring
// lifecycle.Reaper.Tick's Interval+defaultStopTimeout budget (that method's
// own doc comment: "a single wedged StopRun must not pin the lock ... past
// that bound"). The advisory-lock connection and a pool connection are held
// for the tick's duration, so a single wedged Runner.Status,
// stopSandboxOrAudit or revokeRunCascade call must not pin
// db.TerminalSandboxSweepLockKey — and with it, every other control plane's
// sweep — forever. 2 minutes mirrors the reaper's own defaultStopTimeout
// margin, added on top of the interval for the same reason: a child
// context.WithTimeout can only shorten its parent's deadline, so budgeting at
// bare interval would silently cap the per-tick margin at whatever tick time
// was left. A var only so a test can shrink it.
var terminalSandboxSweepTickTimeout = terminalSandboxSweepInterval + 2*time.Minute

// terminalSandboxPager is the *api.Server surface runTerminalSandboxSweeper
// needs, narrowed so a test can drive the paging/offset/wrap arithmetic with
// a fake's worth of runs instead of a real *api.Server and PG.
type terminalSandboxPager interface {
	SweepTerminalSandboxesPage(ctx context.Context, page store.Page) (swept, pageLen int, err error)
}

// runTerminalSandboxSweeper is #710's reaper tick: a periodic, bounded-page
// retry surface for the two SweepTerminalSandboxesPage gaps (a failed
// StopSandbox/RevokeRun step inside a prior finalize/kill, or a
// killTeardownTail interrupted by a shutdown/crash) that nothing else
// revisits automatically — today the only other caller is the
// operator-triggered POST /admin/sandboxes/sweep.
//
// tickLock, when non-nil, makes each tick single-flight across control
// planes, the same contract as lifecycle.Config.TickLock: it must TRY to take
// a cluster-wide lock and return a release func, or (nil, false) when someone
// else holds it, in which case the tick is skipped entirely, not queued.
// Needed because claimSingleInstance (single_instance.go) is not mutual
// exclusion: it holds db.SingleInstanceLockKey for the process lifetime only
// in the DEFAULT configuration — a deployment booted with
// -allow-multi-instance skips that claim, and a Postgres restart/failover can
// release its session under a still-running daemon while a second one boots
// and claims it (SingleInstanceLockKey's own HONEST CEILING). Either way, two
// tickers running at once would both re-run teardown for the same aged KILLED
// run — doubling its run.kill rows and calling the runner twice — exactly the
// hazard reapTickLock exists to prevent for the lifecycle reaper. Production
// wires terminalSandboxSweepTickLock (adapters.go); nil is test-only.
//
// Bounded cost: each tick reads at most terminalSandboxSweepPageSize runs via
// pager.SweepTerminalSandboxesPage (store.Pager.ListRunsPage under it),
// advancing offset by one page per tick and wrapping back to 0 once a page
// comes back shorter than the page size — the previous objection to a ticker
// here (reconcile.go's ReconcileOnBoot doc comment) was that
// SweepTerminalSandboxes calls ListRuns unpaged, so its cost would grow with
// run history forever; paging removes that growth.
//
// time.NewTicker's first tick fires after interval, not immediately, so
// ReconcileOnBoot (a synchronous call within startBackgroundWorkers, and
// orders of magnitude faster than terminalSandboxSweepInterval) always
// completes well before this ticker's first sweep — no explicit ordering with
// it is needed. Every terminal row a page sweeps was already terminal before
// this tick ran; a row ReconcileOnBoot has not yet flipped terminal is simply
// skipped this tick and caught on a later one, exactly like every other pass
// over run state here. startBackgroundWorkers still starts this AFTER calling
// ReconcileOnBoot, so the ordering is structural, not only a timing argument.
func runTerminalSandboxSweeper(ctx context.Context, pager terminalSandboxPager, tickLock func(context.Context) (func(), bool), interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	offset := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			offset = terminalSandboxSweepLockedTick(ctx, pager, tickLock, offset)
		}
	}
}

// terminalSandboxSweepLockedTick is one tick, gated by tickLock when wired —
// mirrors lifecycle.Reaper.Tick's own shape exactly, for the same reason.
// The WHOLE tick (lock acquisition through the sweep's own per-run calls)
// runs under terminalSandboxSweepTickTimeout, applied unconditionally like
// Reaper.Tick's own deadline, and the lock is released via defer immediately
// after it is taken — never called inline after the sweep returns. Without
// both: a panic inside the sweep (caught only by the outer goSafe, which ends
// this loop for the process) unwinds past an inline release and leaves the
// advisory-lock connection checked out forever, and a wedged
// Runner.Status/stopSandboxOrAudit/revokeRunCascade call pins that same
// connection until it returns — either way holding
// db.TerminalSandboxSweepLockKey, and with it every other control plane's
// sweep, indefinitely.
func terminalSandboxSweepLockedTick(ctx context.Context, pager terminalSandboxPager, tickLock func(context.Context) (func(), bool), offset int) int {
	ctx, cancel := context.WithTimeout(ctx, terminalSandboxSweepTickTimeout)
	defer cancel()
	if tickLock != nil {
		release, ok := tickLock(ctx)
		if !ok {
			return offset // another control plane holds the lock this tick; skip, don't queue
		}
		defer release()
	}
	return terminalSandboxSweepTick(ctx, pager, offset)
}

// terminalSandboxSweepTick runs one page and returns the next tick's offset:
// advanced by one page, or wrapped to 0 once a page comes back shorter than
// terminalSandboxSweepPageSize (the end of the table). A failed tick keeps
// the current offset, so the same page is retried next time rather than
// silently skipped.
func terminalSandboxSweepTick(ctx context.Context, pager terminalSandboxPager, offset int) int {
	swept, pageLen, err := pager.SweepTerminalSandboxesPage(ctx, store.Page{Limit: terminalSandboxSweepPageSize, Offset: offset})
	if err != nil {
		slog.WarnContext(ctx, "wardynd: terminal sandbox sweep tick failed", slog.Any("err", err))
		return offset
	}
	if swept > 0 {
		slog.InfoContext(ctx, "wardynd: terminal sandbox sweep tick", slog.Int("swept", swept), slog.Int("offset", offset))
	}
	if pageLen < terminalSandboxSweepPageSize {
		return 0 // short page: reached the end of the table — wrap
	}
	return offset + terminalSandboxSweepPageSize
}

// terminalSandboxBackgrounder is the *api.Server surface
// startTerminalSandboxSweeper needs: the sweep call plus the goBackground
// registration, narrowed for the same reason as terminalSandboxPager.
type terminalSandboxBackgrounder interface {
	terminalSandboxPager
	GoBackground(fn func())
}

// startTerminalSandboxSweeper registers runTerminalSandboxSweeper as
// goBackground-tracked work — srv.GoBackground, not a bare `go` — so
// WaitBackground waits for the ticker to actually exit at shutdown instead of
// abandoning it mid-tick the instant httpSrv.Shutdown returns (the same
// concern runShutdownSequence documents for a launch or a kill-supersede
// teardown; see boot_serve.go). Extracted from startBackgroundWorkers so a
// test can prove that registration without waiting a full interval or
// standing up the rest of it.
func startTerminalSandboxSweeper(ctx context.Context, srv terminalSandboxBackgrounder, tickLock func(context.Context) (func(), bool), interval time.Duration) {
	srv.GoBackground(func() {
		goSafe("terminal_sandbox.sweeper", func() { runTerminalSandboxSweeper(ctx, srv, tickLock, interval) })
	})
	slog.Info("wardynd: terminal sandbox sweeper started", slog.Duration("interval", interval))
}

// terminalSandboxSweeperServer satisfies terminalSandboxBackgrounder;
// asserted here rather than at the one call site so a signature drift on
// either *api.Server method fails to compile right next to this file's own
// interfaces, not inside startBackgroundWorkers.
var _ terminalSandboxBackgrounder = (*api.Server)(nil)
