// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/google/uuid"
)

// inflightCreates holds the cancel func of every run's in-flight
// CreateSandbox, so a kill of a STARTING run stops the create instead of
// leaving it to hold cluster room until its own readiness wait expires.
// Dispatch runs detached (context.WithoutCancel), so nothing else can reach it.
// A kill served by another replica reaches it over NOTIFY (cancelCreate, live_bus.go);
// if that notice is lost, dispatch's STARTING to RUNNING compare still tears the
// sandbox down. Zero value is ready.
type inflightCreates struct{ m sync.Map } // uuid.UUID -> context.CancelFunc

// track registers the run BEFORE dispatch's PENDING->STARTING claim, so a kill
// that wins the run at any point after the claim always finds it. The returned
// context is for CreateSandbox alone: the rollback and compensation after it
// must keep running on the uncancelled dispatch context.
func (c *inflightCreates) track(ctx context.Context, runID uuid.UUID) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	c.m.Store(runID, cancel)
	return ctx, func() {
		c.m.Delete(runID)
		cancel()
	}
}

// cancel stops the run's in-flight create, if it has one. Idempotent.
func (c *inflightCreates) cancel(runID uuid.UUID) {
	if f, ok := c.m.Load(runID); ok {
		f.(context.CancelFunc)()
	}
}

// trackRunBuild registers runID's image build so a kill stops it (cancelCreate). The registration is
// its own (runBuilds): it can neither replace nor remove the one the run's queue start or dispatch
// holds in creates. The returned context is for the builder alone and carries the build's usual
// deadline; recording and compensating the build stay on ctx. The run is re-read after the
// registration, because a kill that landed before it found nothing to cancel: live is false for a
// run that is already terminal, whose build must not start. A run that cannot be read may be one
// of those, so that is errRunUnreadBeforeBuild, and the caller fails the run rather than build or
// dispatch it. Cancelling asks the builder to stop; what a stopped build left on the host is the
// builder's and the image sweep's to remove.
func (s *Server) trackRunBuild(ctx context.Context, runID uuid.UUID) (buildCtx context.Context, end func(), live bool, err error) {
	tracked, untrack := s.runBuilds.track(ctx, runID)
	buildCtx, cancel := context.WithTimeout(tracked, imageBuildTimeout)
	end = func() {
		cancel()
		untrack()
	}
	if s.cfg.ImageBuilder == nil {
		return buildCtx, end, true, nil // nothing can build, so there is no stale work to refuse
	}
	run, err := s.cfg.Store.GetRun(ctx, runID)
	if err != nil {
		end()
		slog.ErrorContext(ctx, "wardynd: the run could not be read before its image build; failing it",
			slog.String("run_id", runID.String()), slog.Any("err", err))
		return buildCtx, end, false, errRunUnreadBeforeBuild
	}
	if isTerminalRunState(run.State) {
		end()
		return buildCtx, end, false, nil
	}
	return buildCtx, end, true, nil
}

// errRunUnreadBeforeBuild is trackRunBuild's answer for a run it could not read. Its text is the
// member-visible failure hint, so the store's own error goes to the log instead.
var errRunUnreadBeforeBuild = errors.New("the run's state could not be read before its image build")
