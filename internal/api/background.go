// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"time"
)

// goBackground runs fn on a goroutine tracked by s.bg (server.go), so an
// orderly shutdown (WaitBackground) can wait for it instead of abandoning it
// the instant http.Server.Shutdown returns; `grep -n goBackground` lists the
// callers. They are of two kinds: work detached from a request (a launch, a
// kill teardown, output persistence, attach-lease and queue follow-ups), which
// ends on its own bound, and the loops ReconcileOnBoot starts (the
// orphaned-build and run-watcher sweepers and each run's watcher), which
// return once BaseCtx is cancelled. New work of either kind routes through
// this instead of a bare `go`.
func (s *Server) goBackground(fn func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
}

// GoBackground is goBackground, exported for cmd/wardynd's own long-lived
// background loops that must not be abandoned mid-cycle by a SIGTERM the way
// a bare `go` would be — today the terminal-sandbox sweep ticker
// (runTerminalSandboxSweeper, #710): fn is expected to run until its ctx is
// cancelled and then return promptly, so WaitBackground's wait for it
// resolves near-instantly once rootCtx is already cancelled (which it always
// is by the time serveAndShutdown calls WaitBackground), never by hitting
// backgroundShutdownBudget the way a genuinely wedged request-detached
// teardown would.
func (s *Server) GoBackground(fn func()) {
	s.goBackground(fn)
}

// backgroundShutdownBudget bounds WaitBackground. killCascadeTimeout
// (runs_lifecycle.go) is the longest bound most goroutines tracked by
// goBackground give themselves internally (killTeardownTail's own
// detach+bound, or finishHarnessLoginLaunch's dispatch). killSignInRunAfterCapture
// (ssotoken.go) is the one exception with an inner bound of its OWN, and it
// loops: each of up to supersedeCASAttempts passes is a run read
// (signInCaptureReadTimeout) plus killRunCascade (killCascadeTimeout, claim and
// teardown under one shared bound), so the strict ceiling is
// supersedeCASAttempts * (signInCaptureReadTimeout + killCascadeTimeout), 105s.
// This constant is one pass of that sum. It covers the kill because a pass that
// loses its swap to the launch moving the run forward tears nothing down: it
// costs a read and one row update, then re-reads, so the teardown runs once, in
// the pass that wins. A store where every read and every lost swap ran to its
// full timeout could pass the budget; the goroutine is then abandoned with the
// WARN below and the run is ended by ReconcileOnBoot or its idle cap. The budget
// is not sized to 105s because HTTPShutdownTimeout, this budget and the audit
// flush must fit the 70s termination grace (TestShutdownGraceCoversTheBudget).
//
// finishCreateRunLaunch has no such inner bound: a devcontainer build may take
// imageBuildTimeout (30 minutes). The budget still applies to it, so a shutdown
// during a long build is abandoned with the WARN below, and the run it leaves
// non-terminal is picked up by ReconcileOnBoot's passes (reconcile.go).
const backgroundShutdownBudget = killCascadeTimeout + killRevokeTimeout + signInCaptureReadTimeout

// WaitBackground blocks until every goroutine started through goBackground has
// returned, or until backgroundShutdownBudget elapses — whichever comes first.
// Bounded because this runs on the shutdown path with an operator waiting: a
// genuinely wedged teardown (a runner call that never returns) must not turn
// an orderly stop into a hang. A hit budget is logged rather than silent, so
// an abandoned kill cascade or launch is visible in the log instead of just
// disappearing.
func (s *Server) WaitBackground() {
	budget := backgroundShutdownBudget
	if s.bgWaitBudget > 0 {
		budget = s.bgWaitBudget
	}
	done := make(chan struct{})
	go func() {
		s.bg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(budget):
		slog.Warn("wardynd: shutdown budget hit with background work (a launch, a teardown, output persistence, "+
			"or a sweeper or run watcher that did not stop on cancel) still running; abandoning it", slog.Duration("budget", budget))
	}
}

// HTTPShutdownTimeout bounds cmd/wardynd's http.Server.Shutdown, the step that
// runs before WaitBackground. The two budgets run back to back, so together
// with the audit sinks' final flush they set the shortest grace period
// wardynd needs before SIGKILL. deploy/helm's terminationGracePeriodSeconds
// and deploy/compose's stop_grace_period are 85s for that reason, and
// TestShutdownGraceCoversTheBudget fails if either falls below the sum.
const HTTPShutdownTimeout = 15 * time.Second
