// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// attachDispatchBy is an issuer portal's ticket's dispatch time: the start of the ticket check that admitted the
// opening plus uiDispatchFresh. Its thaw and its first Runner.Attach start before it (uigateway_dispatch.go).
func attachDispatchBy(ctx context.Context) (time.Time, bool) {
	ta, ok := ticketActorFromContext(ctx)
	if !ok || !issuerPortalPerson(ta.via, ta.principal) || ta.checkStart.IsZero() {
		return time.Time{}, false
	}
	return ta.checkStart.Add(uiDispatchFresh()), true
}

// attachDispatchPast reports whether ctx is an issuer portal's opening at or after its dispatch time.
func attachDispatchPast(ctx context.Context) bool {
	by, ok := attachDispatchBy(ctx)
	return ok && !time.Now().Before(by)
}

// attachThawCtx is actx for the opening's thaw: for an issuer portal's ticket (read off ctx), bounded by its
// dispatch time too.
func attachThawCtx(ctx, actx context.Context) (context.Context, context.CancelFunc) {
	by, ok := attachDispatchBy(ctx)
	if !ok {
		return actx, func() {}
	}
	return context.WithDeadline(runner.WithStartCutoff(actx, by), by)
}

// attachPromotionCheck is an issuer portal's socket's own check before an opening its ticket's check does not
// cover (establishExec), nil for any other socket: the socket's authority (attachAuthority, the check its watch
// repeats) once more, under the watch's lookup bound. It returns when it started and whether it passed; one that
// does not pass cuts the socket as the watch does.
func (s *Server) attachPromotionCheck(r *http.Request, runID uuid.UUID, cut *streamCut) func(context.Context) (time.Time, bool) {
	if cut == nil {
		return nil
	}
	check := s.attachAuthority(r, runID)
	return func(ctx context.Context) (time.Time, bool) {
		start := time.Now()
		if cut.lookup(ctx, s.attachAuthorityLookupBound(), check) != nil {
			cut.Cut()
			return start, false
		}
		return start, true
	}
}

// openingGate is establishExec's gate for h: the ticket's dispatch time (attachDispatchBy) and whether h's openings
// are gated. A holder with its own check (an issuer portal's) is gated whatever ctx carries; with no ticket time,
// its first opening is past it.
func (h *attachHolder) openingGate(ctx context.Context) (time.Time, bool) {
	by, ok := attachDispatchBy(ctx)
	return by, ok || h.recheck != nil
}

// openingBy is the dispatch time of h's next opening on an issuer portal's socket (gated): the ticket's (ticketBy)
// for the socket's first opening in the role it was registered in, else the start of h's own check (recheck),
// asked now, plus uiDispatchFresh. A check that does not pass has cut the socket, and one that passes as ctx ends
// (the watch's cut, a close) opens nothing either (errAttachCancelled); a holder with no check of its own is past
// any dispatch time (errAttachTimedOut). Any other socket's is ticketBy, unread.
// The caller holds openMu and read gen after the role.
func (h *attachHolder) openingBy(ctx context.Context, ticketBy time.Time, gated bool, gen uint64) (time.Time, error) {
	if !gated || (!h.execOpened && gen == 0) {
		return ticketBy, nil
	}
	if h.recheck == nil {
		return time.Time{}, errAttachTimedOut
	}
	start, ok := h.recheck(ctx)
	if !ok || ctx.Err() != nil {
		return time.Time{}, errAttachCancelled
	}
	return start.Add(uiDispatchFresh()), nil
}
