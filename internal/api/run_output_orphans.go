// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// Live output chunks with no run_outputs row. The chunk append writes the run's pending row with the
// chunk (store.RunOutputChunkStore), so no chunk is stored without one; a server without that left
// such chunks when a run's dispatch-time pending insert failed and its final row was never written.
// With no row, no recovery selected them and no retention deleted them. The output sweep
// (SweepRunOutputs) now ages and adopts them: the chunks of a run that ended longer ago than the
// retention window are deleted, and every other ended run's get the pending row they were owed,
// which the stale-pending sweep then resolves as it resolves any other, writing the row from the
// chunks. Neither touches a live run's output: both take terminal runs only, which never return to
// live, and a pending row carries no bytes.

// orphanChunkSweepPage is how many runs one sweep gives a pending row.
const orphanChunkSweepPage = 200

// deleteEndedRunOutputChunks deletes the live chunks of runs that ended longer ago than retention, and
// logs how many it deleted. A store that keeps no orphan record deletes none.
func (s *Server) deleteEndedRunOutputChunks(ctx context.Context, retention time.Duration) error {
	orphans, ok := s.cfg.Store.(store.RunOutputOrphanStore)
	if !ok {
		return nil
	}
	n, err := orphans.DeleteEndedRunOutputChunks(ctx, retention)
	if n > 0 {
		slog.InfoContext(ctx, "wardynd: deleted the live output chunks of runs that ended past the retention window", slog.Int("deleted", n))
	}
	return err
}

// adoptOrphanChunks gives one page of ended runs whose chunks have no run_outputs row the pending row
// they were owed. An erased run gets none (InsertPendingRunOutput checks its tombstone), and a tail
// this process holds for it is fenced. A failed insert is tried again on the next sweep.
func (s *Server) adoptOrphanChunks(ctx context.Context, st store.RunOutputStore) error {
	orphans, ok := s.cfg.Store.(store.RunOutputOrphanStore)
	if !ok {
		return nil
	}
	ids, err := orphans.ListOrphanRunOutputChunks(ctx, s.cfg.RunOutputRetention, orphanChunkSweepPage)
	if err != nil {
		return err
	}
	for _, id := range ids {
		switch err := st.InsertPendingRunOutput(ctx, id); {
		case errors.Is(err, store.ErrRunOutputErased):
			s.fenceRunOutput(id)
		case err != nil:
			slog.WarnContext(ctx, "wardynd: could not give a run's orphaned output chunks a pending row; retrying next sweep",
				slog.String("run_id", id.String()), slog.Any("err", err))
		}
	}
	return nil
}
