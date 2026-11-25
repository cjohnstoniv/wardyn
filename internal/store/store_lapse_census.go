// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"fmt"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// LapseCensus is the server's own runs as the lapse sweep's outage rule counts them, all on the database's clock.
type LapseCensus struct {
	Running        int           // R: RUNNING, not lost, runner_id NULL, placement <> 'local'
	NewestRenewAge time.Duration // F: now() - max(token_renewed_at) over R; 0 when Running == 0
	Lapsed         int           // L: runs whose token lapsed inside the window and was still live at that moment
}

// LapseCensusReader is the census read. The api layer type-asserts it, as it does RunLoser.
type LapseCensusReader interface {
	// LapseCensus counts the server's own runs (runner_id NULL, placement not local). life is how long a token
	// may go unrenewed before it lapses; window is how far back a lapse still counts.
	LapseCensus(ctx context.Context, life, window time.Duration) (LapseCensus, error)
}

var _ LapseCensusReader = PG{}

// LapseCensus — see LapseCensusReader. A run's lapse moment is token_renewed_at + life; it counts in L when
// that moment is inside the window and the run was still live at it: RUNNING and not lost, lost to an outage
// at or after it, or ended at or after it (and not kept before it).
func (s PG) LapseCensus(ctx context.Context, life, window time.Duration) (LapseCensus, error) {
	const q = `
SELECT
  count(*) FILTER (WHERE state = $3 AND lost_at IS NULL),
  COALESCE(EXTRACT(EPOCH FROM now() - max(token_renewed_at) FILTER (WHERE state = $3 AND lost_at IS NULL)), 0),
  count(*) FILTER (WHERE token_renewed_at + $1::interval > now() - $2::interval
                     AND token_renewed_at + $1::interval <= now()
                     AND ((state = $3 AND lost_at IS NULL)
                       OR (lost_reason = $4 AND lost_at >= token_renewed_at + $1::interval)
                       OR (ended_at IS NOT NULL AND ended_at >= token_renewed_at + $1::interval
                           AND (lost_at IS NULL OR lost_at >= token_renewed_at + $1::interval))))
FROM agent_runs
WHERE runner_id IS NULL AND placement <> 'local'
  AND (state = $3 OR token_renewed_at > now() - ($1::interval + $2::interval))`
	var c LapseCensus
	var age float64
	if err := s.Pool.QueryRow(ctx, q, life.String(), window.String(), string(types.RunRunning), string(types.LostOutage)).
		Scan(&c.Running, &age, &c.Lapsed); err != nil {
		return LapseCensus{}, fmt.Errorf("store: lapse census: %w", err)
	}
	c.NewestRenewAge = time.Duration(age * float64(time.Second))
	return c, nil
}
