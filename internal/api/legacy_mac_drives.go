// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
	"github.com/cjohnstoniv/wardyn/internal/store"
)

// legacyMacReleaser is the store call that settles a Mac's drives from before the upgrade (store.PG).
type legacyMacReleaser interface {
	ReleaseLegacyMacDrives(ctx context.Context, runnerID uuid.UUID) (int64, error)
}

// macProcessIsNew is the evidence that releases a Mac's pre-upgrade disk drives: the runner's caps, sent on a fresh
// session before its READY, say the process has completed no session before this one (PriorSessions 0). Such a
// process started after every daemon that could have been deleting one of its disks stopped, so no delete of an
// earlier daemon can still be running there. A runner that does not count (PriorSessions absent), a process that
// served before (PriorSessions above 0), and one that sent no macOS VM caps are not evidence.
func macProcessIsNew(caps runnerwire.Caps, ok bool) bool {
	return ok && caps.MacOSVM != nil && caps.MacOSVM.PriorSessions != nil && *caps.MacOSVM.PriorSessions == 0
}

// releaseLegacyMacDrives runs at a fresh session's READY: with the evidence it clears legacy_unsettled on the disk
// drives kept on that runner's Mac, and on no other runner's. The runner id is the authenticated session's, never
// a value the caps carry. A write that fails is tried again, after each of readySweepRetries and then at the last one's
// interval, for as long as the session lives; nothing else clears the mark for a Mac.
func (s *Server) releaseLegacyMacDrives(ctx context.Context, runnerID uuid.UUID, sub interface {
	Caps() (runnerwire.Caps, bool)
}, sameSession func() bool) {
	rel, ok := s.cfg.Store.(legacyMacReleaser)
	if caps, have := sub.Caps(); !ok || !macProcessIsNew(caps, have) {
		return
	}
	for attempt := 0; ; attempt++ {
		n, err := rel.ReleaseLegacyMacDrives(ctx, runnerID)
		if err == nil {
			if n > 0 {
				slog.InfoContext(ctx, "wardynd: a Mac's drives from before the upgrade are settled", slog.String("runner_id", runnerID.String()), slog.Int64("drives", n))
			}
			return
		}
		slog.WarnContext(ctx, "wardynd: settle a Mac's drives from before the upgrade", slog.String("runner_id", runnerID.String()), slog.Any("err", err))
		select {
		case <-ctx.Done():
			return
		case <-time.After(readySweepRetries[min(attempt, len(readySweepRetries)-1)]):
		}
		if ctx.Err() != nil || !sameSession() {
			return
		}
	}
}

var _ legacyMacReleaser = store.PG{}
