// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"cmp"
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The limit watch: a relayed request of a session no other watch holds (the run's owner, a run caller, a browser
// inbound session, a super admin) is asked the re-check's profile arm again every uiReassertInterval, so a governance
// limit set after the request began closes it, a WebSocket's back connection included (#3943). Only a definite
// refusal ends it: a profile or run read that fails keeps the stream and asks again next tick, as such a stream had no
// watch at all before. A helper's session keeps supportWatch, an issuer portal's its streamCut, and an inbound token's
// stream watchInboundStream.

// limitWatchCtx is the context a relayed request of a session with no grant runs under: ctx and a no-op for a request
// an inbound token admitted (watchInboundStream holds its stream), else a cancellable child that limitWatch cancels at
// the first definite refusal. stop ends the watch and must be called when the request is done.
func (s *Server) limitWatchCtx(ctx context.Context, sess uiSession) (context.Context, func()) {
	if _, token := ctx.Value(inboundReadmitCtxKey{}).(inboundReadmission); token {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	go s.limitWatch(ctx, cancel, sess)
	return ctx, cancel
}

// limitWatch asks limitWatchCheck every uiReassertInterval until ctx ends, and on a refusal cancels ctx first, then
// writes the refusal's row.
func (s *Server) limitWatch(ctx context.Context, cancel context.CancelFunc, sess uiSession) {
	tick := time.NewTicker(time.Duration(cmp.Or(supportWatchEveryNS.Load(), int64(uiReassertInterval))))
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		de, ended := s.limitWatchCheck(ctx, sess)
		if !ended {
			continue
		}
		cancel()
		if de == nil { // the run is gone: nothing to name
			return
		}
		actx, acancel := context.WithTimeout(context.WithoutCancel(ctx), inboundReadmitTimeout)
		if sess.Inbound {
			s.auditInboundUnitDenies(actx, sess)
		} else {
			_ = s.uiDenyReassert(sess, de.reason, de.status, de.msg)
		}
		acancel()
		return
	}
}

// limitWatchCheck is one tick: the run read fresh and the profile arm the re-check asks, each bounded by
// supportWatchWait. ended is true for a run that is gone (de nil) or a 403 from the arm (de the refusal); a read
// that fails, a 503 from the arm, or the bound running out is not an end.
func (s *Server) limitWatchCheck(ctx context.Context, sess uiSession) (de *uiDialError, ended bool) {
	ctx, stop := context.WithTimeout(ctx, time.Duration(cmp.Or(supportWatchWaitNS.Load(), int64(supportWatchWait))))
	defer stop()
	run, err := s.cfg.Store.GetRun(ctx, sess.Run)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return nil, true
	case err != nil:
		return nil, false
	}
	if de := s.uiSessionLimitDenial(ctx, sess, run); de != nil && de.status == http.StatusForbidden {
		return de, true
	}
	return nil, false
}

// uiSessionLimitDenial is the profile arm uiSessionArms asks for sess, with no row: the inbound arm for an inbound
// session, the session arm for any other.
func (s *Server) uiSessionLimitDenial(ctx context.Context, sess uiSession, run types.AgentRun) *uiDialError {
	if sess.Inbound {
		return s.uiInboundUnitDenial(ctx, sess, run)
	}
	return s.uiSessionUnitDenial(ctx, sess, run)
}
