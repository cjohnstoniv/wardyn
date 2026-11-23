// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrAuditAppendBusy: every request-path append slot stayed taken for the chain lock's whole wait bound. The
// caller spools the event, as for a lock timeout.
var ErrAuditAppendBusy = errors.New("store: audit append busy: every append slot stayed taken past the chain lock's wait bound")

var auditAppendSlotsByPool sync.Map // *pgxpool.Pool -> chan struct{}

// auditAppendSlots is the process's one slot channel for pool: at most half the pool, at least 1 and at most 4,
// waits on the chain lock at once, so a held lock cannot take every connection the pool has. It is sized when
// first used, as guardDialSlots is.
func auditAppendSlots(pool *pgxpool.Pool) chan struct{} {
	if v, ok := auditAppendSlotsByPool.Load(pool); ok {
		return v.(chan struct{}) // the common path: no config copy and no channel per append
	}
	n := max(1, min(4, int(pool.Config().MaxConns)/2))
	v, _ := auditAppendSlotsByPool.LoadOrStore(pool, make(chan struct{}, n))
	return v.(chan struct{})
}

// takeAuditAppendSlot waits up to budget for a slot. It answers the release and the budget left for the lock
// wait. A free slot costs no timer, so a zero or expired budget with a free slot is deterministic.
func takeAuditAppendSlot(ctx context.Context, pool *pgxpool.Pool, budget time.Duration) (release func(), left time.Duration, err error) {
	slots := auditAppendSlots(pool)
	release = func() { <-slots }
	select {
	case slots <- struct{}{}:
		return release, budget, nil
	default:
	}
	start := time.Now()
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case slots <- struct{}{}:
		return release, max(0, budget-time.Since(start)), nil
	case <-timer.C:
		return nil, 0, fmt.Errorf("%w (waited %s)", ErrAuditAppendBusy, budget)
	case <-ctx.Done():
		return nil, 0, ctx.Err()
	}
}
