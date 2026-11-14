// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// sweepDesktopSignins ends every sign-in whose run ended, whose window
// passed, or whose end could not finish before. One pass of the reconciler.
func (s *Server) sweepDesktopSignins(ctx context.Context) error {
	st, on := s.desktopSigninStore()
	if !on {
		return nil
	}
	ctx, end, ok := s.beginLeaderSweep(ctx)
	if !ok {
		return nil
	}
	defer end()
	open, err := st.ListOpenDesktopSignins(ctx)
	if err != nil {
		return err
	}
	now := s.cfg.Now()
	for _, si := range open {
		run, err := s.cfg.Store.GetRun(ctx, si.RunID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err == nil && run.State == types.RunKilled && run.SandboxRef != "" && s.cfg.Runner != nil && signinKillOwed(si.EndStage) {
			// A stuck sign-in's kill that left the sandbox: the row closes once the person's display,
			// in that sandbox, is gone.
			kctx, cancel := context.WithTimeout(ctx, killCascadeTimeout)
			kerr := s.cfg.Runner.KillSandbox(kctx, run.SandboxRef)
			cancel()
			if kerr != nil {
				slog.WarnContext(ctx, "wardynd: desktop sign-in's killed sandbox is not torn down", "run_id", run.ID, "err", kerr)
				continue
			}
			_ = s.closeDesktopSignin(ctx, st, run, si, si.EndRequested, types.ActorSystem, "wardynd", false)
			continue
		}
		if err != nil || run.State != types.RunRunning || run.SandboxRef == "" || runIsKept(run) {
			_ = s.closeDesktopSignin(ctx, st, run, si, signinEndRunEnded, types.ActorSystem, "wardynd", false)
			continue
		}
		reason := si.EndRequested
		if reason == "" && !now.Before(si.ExpiresAt) {
			reason = signinEndExpired
		}
		if reason != "" {
			_ = s.finishDesktopSignin(ctx, run, si, reason, types.ActorSystem, "wardynd")
		}
	}
	return nil
}

// inDesktopSignin reports whether the run has an open sign-in, reading the
// set once per sweep into *cache. An unreadable set counts every run in one.
func (s *Server) inDesktopSignin(ctx context.Context, cache *map[uuid.UUID]bool, runID uuid.UUID) bool {
	st, on := s.desktopSigninStore()
	if !on {
		return false
	}
	if *cache == nil {
		m, err := st.RunsWithOpenDesktopSignin(ctx)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: open desktop sign-ins unreadable; no run is frozen this pass", "err", err)
			return true
		}
		*cache = m
	}
	return (*cache)[runID]
}
