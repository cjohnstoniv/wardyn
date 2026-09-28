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
// No leader election needed: claimSingleInstance (single_instance.go) already
// holds db.SingleInstanceLockKey for the WHOLE process lifetime, claimed in
// main() before startBackgroundWorkers (and every goroutine it starts) ever
// runs — so at most one wardynd runs against a database at a time, the exact
// property a per-tick advisory lock (the lifecycle reaper's reapTickLock)
// exists to provide a reaper with no process-lifetime lock of its own. This
// one already has one.
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
// over run state here.
func runTerminalSandboxSweeper(ctx context.Context, pager terminalSandboxPager, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	offset := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			swept, pageLen, err := pager.SweepTerminalSandboxesPage(ctx, store.Page{Limit: terminalSandboxSweepPageSize, Offset: offset})
			if err != nil {
				slog.WarnContext(ctx, "wardynd: terminal sandbox sweep tick failed", slog.Any("err", err))
				continue
			}
			if swept > 0 {
				slog.InfoContext(ctx, "wardynd: terminal sandbox sweep tick", slog.Int("swept", swept), slog.Int("offset", offset))
			}
			if pageLen < terminalSandboxSweepPageSize {
				offset = 0 // short page: reached the end of the table — wrap
			} else {
				offset += terminalSandboxSweepPageSize
			}
		}
	}
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
func startTerminalSandboxSweeper(ctx context.Context, srv terminalSandboxBackgrounder, interval time.Duration) {
	srv.GoBackground(func() {
		goSafe("terminal_sandbox.sweeper", func() { runTerminalSandboxSweeper(ctx, srv, interval) })
	})
	slog.Info("wardynd: terminal sandbox sweeper started", slog.Duration("interval", interval))
}

// terminalSandboxSweeperServer satisfies terminalSandboxBackgrounder;
// asserted here rather than at the one call site so a signature drift on
// either *api.Server method fails to compile right next to this file's own
// interfaces, not inside startBackgroundWorkers.
var _ terminalSandboxBackgrounder = (*api.Server)(nil)
