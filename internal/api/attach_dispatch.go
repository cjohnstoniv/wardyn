// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// attachDispatchBy is an issuer portal's ticket's dispatch time: the start of the ticket check that admitted the
// opening plus uiDispatchFresh. Its thaw and its first Runner.Attach start before it (uigateway_dispatch.go).
func attachDispatchBy(ctx context.Context) (time.Time, bool) {
	ta, ok := ticketActorFromContext(ctx)
	if !ok || !issuerPortalPerson(ta.via, ta.principal) || ta.checkStart.IsZero() {
		return time.Time{}, false
	}
	return ta.checkStart.Add(uiDispatchFresh), true
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
