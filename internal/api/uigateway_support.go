// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// The UI gateway's side of a support grant (support_entry.go holds the arm). A helper's ticket carries the grant
// (ticketActor.support), the session copies it (uiSession.SupportGrant) and every re-check asks the arm again
// (uiSessionStillAuthorized). What the re-check cannot reach is a request that is already being relayed: a
// WebSocket or an event stream is one request for its whole life, so supportWatchCtx ends it.

// supportWatchWait bounds one watch check, so a store that hangs ends the stream instead of leaving it open.
const supportWatchWait = 5 * time.Second

// supportWatchEveryNS and supportWatchWaitNS replace uiReassertInterval and supportWatchWait when set: tests only,
// so a revoke can be observed without a 30 second wait.
var supportWatchEveryNS, supportWatchWaitNS atomic.Int64

// uiSupportDoor is the grant door app is behind: the desktop for a desktop (a VM's display, a declared noVNC),
// web_apps for any other app.
func uiSupportDoor(app string) supportDoor {
	if uiAppIsDesktopSession(app) {
		return supportDoorDesktop
	}
	return supportDoorWebApps
}

// supportWatchCtx is the context a helper's relayed request runs under: ctx and a no-op unless sess carries a
// grant, else a cancellable child that a watch cancels at the first re-check that refuses (or, for one with no
// answer, once the session budget runs out), so the reverse proxy closes a 101's back connection or aborts a
// streaming body (net/http/httputil closes a switched connection when the request's context ends). stop ends
// the watch and must be called when the request is done.
// An owner's request is never watched, as the check would only repeat the ownership rule.
func (s *Server) supportWatchCtx(ctx context.Context, sess uiSession) (context.Context, func()) {
	if sess.SupportGrant == uuid.Nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go s.supportWatch(ctx, cancel, sess)
	return ctx, cancel
}

// supportWatch re-checks sess every uiReassertInterval until ctx ends, and cancels it when a check refuses it or
// no check has answered for the session budget. Nothing is checked as the stream starts: the request's own
// re-assert has just run, as a helper's is never debounced (uiReassertDue), so a stream opened after a revoke was
// refused before it was relayed.
func (s *Server) supportWatch(ctx context.Context, cancel context.CancelFunc, sess uiSession) {
	tick := time.NewTicker(time.Duration(cmp.Or(supportWatchEveryNS.Load(), int64(uiReassertInterval))))
	defer tick.Stop()
	b := s.newSessionBudget(ctx, uiWatchID(sess, sessionSurfaceUIApp))
	defer b.leave()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tickOrRetry(tick.C, b):
		}
		err := s.supportWatchCheck(ctx, sess)
		if ctx.Err() != nil {
			return // the stream ended on its own meanwhile
		}
		if v := b.observe("authority", err); v.ends() {
			if v == budgetSpent {
				b.end()
			}
			cancel()
			return
		}
	}
}

// supportWatchCheck loads sess's run and asks uiSessionStillAuthorized, which asks the arm. A denial answers
// errRelayAuthorityEnded; a run that is gone does too. A failed read, a denial that is itself a read that failed
// (503) and a check still running when its bound ends answer an unconfirmed error, which rides the session budget.
func (s *Server) supportWatchCheck(ctx context.Context, sess uiSession) error {
	ctx, stop := context.WithTimeout(ctx, time.Duration(cmp.Or(supportWatchWaitNS.Load(), int64(supportWatchWait))))
	defer stop()
	held := make(chan error, 1) // buffered: a check that outlives its bound still has somewhere to answer
	go func() {
		run, err := s.cfg.Store.GetRun(ctx, sess.Run)
		if errors.Is(err, store.ErrNotFound) {
			held <- errRelayAuthorityEnded
			return
		}
		if err != nil {
			slog.WarnContext(ctx, "wardynd: support watch could not read the run", "run_id", sess.Run, "err", err)
			held <- unconfirmed(err)
			return
		}
		switch de := s.uiSessionStillAuthorized(ctx, sess, run); {
		case de == nil:
			held <- nil
		case de.status == http.StatusServiceUnavailable:
			held <- unconfirmed(errRelayAuthorityEnded)
		default:
			held <- errRelayAuthorityEnded
		}
	}()
	select {
	case err := <-held:
		return err
	case <-ctx.Done():
		return unconfirmed(ctx.Err())
	}
}
