// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"time"

	"github.com/google/uuid"
)

// RunSecretGrace is how long a run must have been terminal before its masking
// corpus is evicted. It is a GRACE, not a retention policy: the late readers
// secretmask.Registry.Evict names (the audit recorder masking a finalize event,
// a teardown_error from a retried sandbox sweep, a recording uploaded as the
// session closes) all run within seconds-to-minutes of the terminal
// transition, and masking fails OPEN, so evicting too early unmasks exactly the
// events most likely to quote a credential. An hour buys those readers room
// while still bounding how long plaintext lives in daemon memory.
const RunSecretGrace = time.Hour

// SweepRunSecrets drops the per-run plaintext secret corpora held in memory for
// runs that went terminal more than RunSecretGrace ago, and reports how many it
// evicted.
//
// Eligibility is read from the run's own state rather than hooked onto a
// terminal-transition call site, so every path that ends a run (watcher, kill,
// idle reaper, boot reconcile, failAndRevoke) is covered by construction and no
// new one can be forgotten. It fails CLOSED on anything it cannot positively
// prove terminal-and-cold: a store error, or a held run id with no row in the
// listing, keeps its secrets.
//
// It also drops the process-wide mask copies of credentials that were refreshed
// or deleted more than RunSecretGrace ago (secretmask.Registry.SweepGlobals),
// on the same grace and for the same reason.
//
// The error is the run listing's: a sweep that could not look at the runs
// evicted nothing, and says so, so that it is not counted as a clean tick.
func (s *Server) SweepRunSecrets(ctx context.Context) (int, error) {
	if s.cfg.MaskRegistry == nil {
		return 0, nil
	}
	s.cfg.MaskRegistry.SweepGlobals(s.cfg.Now().Add(-RunSecretGrace))
	if s.cfg.MaskManifests != nil {
		s.cfg.MaskManifests.DropFenced(ctx)
	}
	if s.cfg.Store == nil {
		return 0, nil
	}
	// The committed rows are the leader's: one replica deletes them, and every
	// other replica drops its cache when it reads the tombstones. A follower's
	// pass below touches only its own cache.
	lctx, end, lead := s.beginLeaderSweep(ctx)
	if lead {
		defer end()
	}
	held := s.cfg.MaskRegistry.RunIDs()
	var persisted []uuid.UUID
	if lead {
		var perr error
		if persisted, perr = s.cfg.MaskRegistry.PersistedRuns(lctx); perr != nil {
			return 0, fmt.Errorf("list the runs with committed masking state: %w", perr)
		}
	}
	if len(held) == 0 && len(persisted) == 0 {
		return 0, s.sweepCommittedMasks(lctx, lead, nil)
	}
	byID, err := s.runsByID(ctx, slices.Concat(held, persisted))
	if err != nil {
		slog.WarnContext(ctx, "wardynd: run-secret sweep skipped", slog.Any("err", err))
		return 0, fmt.Errorf("list runs for the run-secret sweep: %w", err)
	}
	now, err := s.dbNow(ctx) // against updated_at, the database's stamp
	if err != nil {
		return 0, fmt.Errorf("read the database's clock for the run-secret sweep: %w", err) // nothing evicted
	}
	cutoff := now.Add(-RunSecretGrace)
	cold := func(id uuid.UUID) bool {
		run, known := byID[id]
		return known && isTerminalRunState(run.State) && run.UpdatedAt.Before(cutoff)
	}
	evicted := 0
	for _, id := range held {
		if !cold(id) {
			continue
		}
		s.cfg.MaskRegistry.Evict(id)
		s.forgetMaskManifest(id)
		evicted++
	}
	return evicted, s.sweepCommittedMasks(lctx, lead, slices.DeleteFunc(persisted, func(id uuid.UUID) bool { return !cold(id) }))
}
