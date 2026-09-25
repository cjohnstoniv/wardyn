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
// of a bare `go`. The callers today are handleCreateRun's launch
// (finishCreateRunLaunch, runs_create_launch.go), finishHarnessLoginLaunch's
// launch (harnesscred_launch.go), supersedeOneLoginRun's kill-teardown tail
// (harnesscred_supersede.go) and killSignInRunAfterCapture (ssotoken.go).
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
//
// finishCreateRunLaunch has no such inner bound: a devcontainer build may take
// imageBuildTimeout (30 minutes). The budget still applies to it, so a shutdown
// during a long build is abandoned with the WARN below, and the run it leaves
// non-terminal is picked up by ReconcileOnBoot's passes (reconcile.go).
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
