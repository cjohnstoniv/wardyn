// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// macWindowRecheckWait bounds one re-check of an open window: every store read it makes shares it. A database that
// stops answering then reads as a check that got no answer, the same as one that refuses connections, and the
// watch goes on to see the runner end the window, wardynd go down, or the session budget run out. It is under the
// 30 s re-check interval, so a stalled check never runs into the next one. A var so a test can shorten it;
// TestMacWindowRecheckWaitStaysUnderTheInterval pins the production value.
var macWindowRecheckWait = 10 * time.Second

// macWindowRecheck asks again what the open asked: the run as it stands now, the person's credential and the
// rows of macWindowRefusal, with the same own-runner rule (no delegate, no service identity, no operator-owned
// run). It returns the close reason, "" while the window may stay, the check that refused, and whether that is
// a read that failed. Anything it cannot read is unread: the window rides the session budget, then ends as
// refused_on_recheck. A window that stays stamps the run's presence and touches the run, so an open window
// keeps the run from idling out from under the person. A window that ends touches nothing. Its reads give up together after macWindowRecheckWait.
func (s *Server) macWindowRecheck(ctx context.Context, w *macWindow) (reason, by string, unread bool) {
	ctx, cancel := context.WithTimeout(ctx, macWindowRecheckWait)
	defer cancel()
	run, err := s.cfg.Store.GetRun(ctx, w.runID)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not re-check a window", "run_id", w.runID, "err", err)
		return macWindowRefusedRecheck, reasonRunUnreadable, !errors.Is(err, store.ErrNotFound)
	}
	if isTerminalRunState(run.State) || runIsKept(run) {
		return macWindowRunEnded, "", false
	}
	revoked, err := s.macWindowRevoked(ctx, w.auth)
	if err != nil {
		slog.ErrorContext(ctx, "wardynd: could not re-check a window's credential", "run_id", w.runID, "err", err)
		return macWindowRefusedRecheck, uiDeniedReasonRevocationUnavailable, true
	}
	if revoked {
		return macWindowRevoked, "", false
	}
	if run.RunnerID == nil || *run.RunnerID != w.runnerID {
		return macWindowRefusedRecheck, reasonMacWindowNotYourMac, false
	}
	rf := s.macWindowRefusal(ctx, run, w.auth, macWindowAsk{reopen: true})
	switch {
	case rf == nil:
		_, _ = s.stampPresence(ctx, w.runID, types.ActorHuman, w.auth.Principal, macWindowSurface)
		_ = s.cfg.Store.TouchRun(ctx, w.runID) // as attachKeepalive: an error only leaves the run to the reaper
		return "", "", false
	case rf.err == nil && rf.decision == nil && (rf.body.Reason == reasonMacWindowOffline || rf.body.Reason == reasonMacWindowNotHere):
		return macWindowRunnerOffline, rf.body.Reason, false
	}
	return macWindowRefusedRecheck, macWindowRefusalReason(rf), rf.err != nil && unitReadFailed(rf.err)
}
