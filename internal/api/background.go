// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"log/slog"
	"time"
)

// goBackground runs fn on a goroutine tracked by s.bg (server.go), so an
// orderly shutdown (WaitBackground) can wait for it instead of abandoning it
// the instant http.Server.Shutdown returns. Every caller that detaches work
// from a request with `go s.something(...)` should route through this instead
// of a bare `go` — finishHarnessLoginLaunch's launch (harnesscred_launch.go)
// and supersedeOneLoginRun's kill-teardown tail (harnesscred_supersede.go) are
// the two callers today.
func (s *Server) goBackground(fn func()) {
	s.bg.Add(1)
	go func() {
		defer s.bg.Done()
		fn()
	}()
}

// backgroundShutdownBudget bounds WaitBackground. killCascadeTimeout
// (runs_lifecycle.go) is the longest bound any ONE goroutine tracked by
// goBackground gives itself internally (killTeardownTail's own detach+bound,
// or finishHarnessLoginLaunch's dispatch), plus a margin for the small
// synchronous work around it (the CAS, the audit writes) that killCascadeTimeout
// itself does not cover.
const backgroundShutdownBudget = killCascadeTimeout + 5*time.Second

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
		slog.Warn("wardynd: shutdown budget hit with detached work (a launch or a kill-supersede teardown) "+
			"still running; abandoning it", slog.Duration("budget", budget))
	}
}

// HTTPShutdownTimeout bounds cmd/wardynd's http.Server.Shutdown, the step that
// runs before WaitBackground. The two budgets run back to back, so together
// with the audit sinks' final flush they set the shortest grace period
// wardynd needs before SIGKILL. deploy/helm's terminationGracePeriodSeconds
// and deploy/compose's stop_grace_period are 60s for that reason, and
// TestShutdownGraceCoversTheBudget fails if either falls below the sum.
const HTTPShutdownTimeout = 15 * time.Second
