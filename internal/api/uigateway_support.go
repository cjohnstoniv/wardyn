// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
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
// grant, else a cancellable child that a watch cancels at the first re-check that does not pass, so the reverse
// proxy closes a 101's back connection or aborts a streaming body (net/http/httputil closes a switched
// connection when the request's context ends). stop ends the watch and must be called when the request is done.
// An owner's request is never watched, as the check would only repeat the ownership rule.
func (s *Server) supportWatchCtx(ctx context.Context, sess uiSession) (context.Context, func()) {
	if sess.SupportGrant == uuid.Nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go s.supportWatch(ctx, cancel, sess)
	return ctx, cancel
}

// supportWatch re-checks sess once as the stream starts and then every uiReassertInterval until ctx ends, and
// cancels it when a check does not pass. The first check is the one the request's own re-assert may have
// skipped (that is debounced for uiReassertInterval after the last one), so a stream opened just after a revoke
// ends within one check, not at the first tick. It is bounded as every check is, by supportWatchWait.
func (s *Server) supportWatch(ctx context.Context, cancel context.CancelFunc, sess uiSession) {
	tick := time.NewTicker(time.Duration(cmp.Or(supportWatchEveryNS.Load(), int64(uiReassertInterval))))
	defer tick.Stop()
	for {
		if !s.supportWatchHolds(ctx, sess) {
			cancel()
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// supportWatchHolds loads sess's run and asks uiSessionStillAuthorized, which asks the arm. A denial, a failed
// read and a check still running when its bound ends all answer false (fail closed); ctx ending does too, and
// the caller then cancels what is already cancelled.
func (s *Server) supportWatchHolds(ctx context.Context, sess uiSession) bool {
	ctx, stop := context.WithTimeout(ctx, time.Duration(cmp.Or(supportWatchWaitNS.Load(), int64(supportWatchWait))))
	defer stop()
	held := make(chan bool, 1) // buffered: a check that outlives its bound still has somewhere to answer
	go func() {
		run, err := s.cfg.Store.GetRun(ctx, sess.Run)
		if err != nil {
			slog.WarnContext(ctx, "wardynd: support watch could not read the run; closing the stream", "run_id", sess.Run, "err", err)
			held <- false
			return
		}
		held <- s.uiSessionStillAuthorized(ctx, sess, run) == nil
	}()
	select {
	case ok := <-held:
		return ok
	case <-ctx.Done():
		return false
	}
}
