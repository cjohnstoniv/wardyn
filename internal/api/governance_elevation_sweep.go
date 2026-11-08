// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"log/slog"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// elevationSweepBatch is how many ended elevations one sweep transaction deletes.
const elevationSweepBatch = 200

// sweepExpiredElevations deletes the memberships whose end has passed and writes one
// governance.elevation.expire per row. The reads already ignore an ended row, so the sweep only tidies
// and audits; a run launched under one ends at its own capped lifetime. A store without the sweep has
// nothing to do.
func (s *Server) sweepExpiredElevations(ctx context.Context) error {
	sw, ok := s.cfg.Store.(store.ElevationSweeper)
	if !ok {
		return nil
	}
	for {
		rows, err := sw.SweepExpiredMemberships(ctx, s.cfg.Now().UTC(), elevationSweepBatch)
		if err != nil {
			return err
		}
		for _, m := range rows {
			var by *types.GovernanceChange
			key := membershipKey(m.UnitID, m.SubjectType, m.Subject)
			if err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (err error) {
				by, err = store.LastAppliedGovernanceChangeQ(ctx, q, types.GovernanceTargetElevation, key)
				return err
			}); err != nil {
				// The row is gone either way: write the end without the approval it came from.
				slog.WarnContext(ctx, "wardynd: read the approval of an ended elevation", slog.Any("err", err))
			}
			s.auditElevationEnd(ctx, m, by)
		}
		if len(rows) < elevationSweepBatch {
			return nil
		}
	}
}
