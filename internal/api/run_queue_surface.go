// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// What the pool queue shows a person (docs/design/0.9/pool-queue.md): a pool's advisory slot counts on
// the pool list and the two dry doors, and a waiting run's place in its queue. Counts only: no run id,
// owner or executor ever leaves through either.

// slotCounter is the PG-only read of a pool's counts; a store without it (a test double) shows none.
type slotCounter interface {
	RunnerPoolSlotCounts(ctx context.Context, poolIDs []uuid.UUID) (map[uuid.UUID]store.PoolCounts, error)
}

// queueFactReader is the PG-only read of a waiting run's place.
type queueFactReader interface {
	RunQueueFacts(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]store.RunQueueFact, error)
}

var (
	_ slotCounter     = store.PG{}
	_ queueFactReader = store.PG{}
)

// slotsVerdict is "waits" iff the pool has a cap and is at it or has runs waiting (the queue is first
// come first served, so a free slot behind a waiting run is not yours); else "starts_now". Advisory:
// the launch decides.
func slotsVerdict(max *int, used, waiting int) string {
	if max != nil && (used >= *max || waiting > 0) {
		return client.SlotsWaits
	}
	return client.SlotsStartsNow
}

// poolSlots is one pool's slot answer. maxWaitSec 0 leaves max_wait_sec off.
func poolSlots(pool types.RunnerPool, c store.PoolCounts, maxWaitSec int) *client.PoolSlots {
	var max *int
	if pool.Limits != nil && pool.Limits.MaxConcurrentRuns != nil {
		n := *pool.Limits.MaxConcurrentRuns
		max = &n
	}
	return &client.PoolSlots{Used: c.Used, Waiting: c.Waiting, Max: max, Verdict: slotsVerdict(max, c.Used, c.Waiting), MaxWaitSec: maxWaitSec}
}

// defaultMaxWaitSec is the wait a run of this type gets when it names none.
func defaultMaxWaitSec(rt types.RunnerPoolRunType) int {
	if rt == types.RunnerPoolRunInteractive {
		return queueInteractiveWaitSec
	}
	return queueBackgroundWaitSec
}

// slotCounts reads the counts of poolIDs in one grouped read, outside any lock. The counts are advisory,
// so a failed read is logged and the answer is served without them (nil), never refused. nil too for a
// store that cannot count.
func (s *Server) slotCounts(ctx context.Context, poolIDs []uuid.UUID) map[uuid.UUID]store.PoolCounts {
	sc, ok := s.cfg.Store.(slotCounter)
	if !ok || len(poolIDs) == 0 {
		return nil
	}
	counts, err := sc.RunnerPoolSlotCounts(ctx, poolIDs)
	if err != nil {
		slog.WarnContext(ctx, "wardynd: count runner pool slots", slog.Any("err", err))
		return nil
	}
	return counts
}

// runnerPoolSlots is the resolved pool's slot answer on the dry doors, with the wait this run would
// be given; nil for an unpooled run, a store that cannot count, or a failed count.
func (s *Server) runnerPoolSlots(ctx context.Context, p runPlacement, req createRunRequest) *client.PoolSlots {
	if !p.pooled {
		return nil
	}
	c, ok := s.slotCounts(ctx, []uuid.UUID{p.pool.ID})[p.pool.ID]
	if !ok {
		return nil
	}
	interactive := runTypeForEnd(req.Experience) == types.RunnerPoolRunInteractive || (req.Experience == "" && requestIsInteractive(req))
	wait, _ := queueWaitSec(req, interactive)
	return poolSlots(p.row, c, wait)
}

// projectRunQueue fills AgentRun.queue on each QUEUED run of runs from its queue row, in one read. It is
// derived at read and never stored. The reason is the run's wait token (runner_pool_at_capacity until
// the queue says another). A run that left the queue between the two reads carries none.
func (s *Server) projectRunQueue(ctx context.Context, runs []types.AgentRun) error {
	var ids []uuid.UUID
	for i := range runs {
		if runs[i].State == types.RunQueued {
			ids = append(ids, runs[i].ID)
		}
	}
	qr, ok := s.cfg.Store.(queueFactReader)
	if !ok || len(ids) == 0 {
		return nil
	}
	facts, err := qr.RunQueueFacts(ctx, ids)
	if err != nil {
		return fmt.Errorf("read run queue facts: %w", err)
	}
	for i := range runs {
		f, ok := facts[runs[i].ID]
		if !ok || runs[i].State != types.RunQueued {
			continue
		}
		reason := runs[i].StatusReason
		if !runnerpool.WaitReason(reason).Valid() {
			reason = string(runnerpool.WaitAtCapacity)
		}
		runs[i].Queue = &types.RunQueue{PoolID: f.PoolID, QueuedAt: f.QueuedAt, DeadlineAt: f.DeadlineAt, Position: f.Ahead, Reason: reason}
	}
	return nil
}
