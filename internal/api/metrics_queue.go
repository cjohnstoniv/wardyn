// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

// How a run that waited for a slot left the queue, the closed label set of
// wardyn_runner_pool_queue_wait_seconds.
const (
	queueOutcomeStarted   = "started"
	queueOutcomeTimeout   = "timeout"
	queueOutcomeCancelled = "cancelled"
	queueOutcomeRefused   = "refused"
)

var queueOutcomes = []string{queueOutcomeStarted, queueOutcomeTimeout, queueOutcomeCancelled, queueOutcomeRefused}

// queueWaitBuckets are the histogram's upper bounds in seconds: from a quick hand-off to the longest
// default wait (a day).
var queueWaitBuckets = []float64{1, 5, 30, 60, 300, 900, 3600, 14400, 86400}

// queueWaitHist is wardyn_runner_pool_queue_wait_seconds: how long runs waited, by how they left.
// The zero value is ready.
type queueWaitHist struct {
	counts map[string][]int64 // per outcome, one slot per bucket
	sum    map[string]float64
	n      map[string]int64
}

// queueWait records one run's wait.
func (m *metrics) queueWait(outcome string, d time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	h := &m.queueHist
	if h.counts == nil {
		h.counts, h.sum, h.n = map[string][]int64{}, map[string]float64{}, map[string]int64{}
	}
	if h.counts[outcome] == nil {
		h.counts[outcome] = make([]int64, len(queueWaitBuckets))
	}
	sec := d.Seconds()
	for i, le := range queueWaitBuckets {
		if sec <= le {
			h.counts[outcome][i]++
		}
	}
	h.sum[outcome] += sec
	h.n[outcome]++
}

// writeQueue appends the histogram; the caller holds m.mu.
func (h *queueWaitHist) write(w io.Writer) {
	fmt.Fprint(w, "# HELP wardyn_runner_pool_queue_wait_seconds How long runs waited in a runner pool's queue, by how they left it (started, timeout, cancelled, refused).\n"+
		"# TYPE wardyn_runner_pool_queue_wait_seconds histogram\n")
	for _, outcome := range queueOutcomes {
		for i, le := range queueWaitBuckets {
			var c int64
			if h.counts[outcome] != nil {
				c = h.counts[outcome][i]
			}
			fmt.Fprintf(w, "wardyn_runner_pool_queue_wait_seconds_bucket{outcome=%q,le=\"%g\"} %d\n", outcome, le, c)
		}
		fmt.Fprintf(w, "wardyn_runner_pool_queue_wait_seconds_bucket{outcome=%q,le=\"+Inf\"} %d\n", outcome, h.n[outcome])
		fmt.Fprintf(w, "wardyn_runner_pool_queue_wait_seconds_sum{outcome=%q} %g\n", outcome, h.sum[outcome])
		fmt.Fprintf(w, "wardyn_runner_pool_queue_wait_seconds_count{outcome=%q} %d\n", outcome, h.n[outcome])
	}
}

// runQueueState is the pool queue's in-process state on the Server: the promotion kicks coalesce through
// kick, and gauges is the 15-second snapshot behind the queue gauges on /metrics.
type runQueueState struct {
	kick   queueKicker
	gauges queueSnapshot
}

// queueSnapshot caches the per-pool queue depth and slots in use for fleetSnapshotTTL, shared by
// concurrent scrapes. The zero value is ready.
type queueSnapshot struct {
	mu    sync.Mutex
	at    time.Time
	depth map[uuid.UUID]int
	slots map[uuid.UUID]int
}

func (q *queueSnapshot) get(ctx context.Context, qs queueStore) (depth, slots map[uuid.UUID]int, ok bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.at.IsZero() && time.Since(q.at) < fleetSnapshotTTL {
		return q.depth, q.slots, true
	}
	d, err := qs.QueueDepth(ctx)
	if err != nil {
		q.at = time.Time{}
		return nil, nil, false
	}
	u, err := qs.PoolSlotsUsed(ctx)
	if err != nil {
		q.at = time.Time{}
		return nil, nil, false
	}
	q.at, q.depth, q.slots = time.Now(), d, u
	return d, u, true
}

// writeQueueGauges appends the two per-pool gauges from the 15-second snapshot; on a failed refresh
// they are omitted, never written as zero, like the fleet gauges.
func (s *Server) writeQueueGauges(r *http.Request, w io.Writer) {
	qs, ok := s.queueStore()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storePingTimeout)
	defer cancel()
	depth, slots, ok := s.queue.gauges.get(ctx, qs)
	if !ok {
		return
	}
	for _, g := range []struct {
		name, help string
		values     map[uuid.UUID]int
	}{
		{"wardyn_runner_pool_queue_depth", "Runs waiting in each runner pool's queue, from a 15-second snapshot. Aggregate across replicas with max(), never sum().", depth},
		{"wardyn_runner_pool_slots_used", "Runs holding a slot in each runner pool, from a 15-second snapshot. Aggregate across replicas with max(), never sum().", slots},
	} {
		fmt.Fprintf(w, "# HELP %s %s\n# TYPE %s gauge\n", g.name, g.help, g.name)
		ids := slices.SortedFunc(maps.Keys(g.values), func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
		for _, id := range ids {
			fmt.Fprintf(w, "%s{pool_id=%q} %d\n", g.name, id.String(), g.values[id])
		}
	}
}

// queueKicker coalesces promotion kicks: one runs at a time, and kicks that arrive meanwhile make it go
// round once more.
type queueKicker struct{ running, pending atomic.Bool }

// start claims the right to run; false means one is already running and has been told to go again.
func (k *queueKicker) start() bool {
	if k.running.CompareAndSwap(false, true) {
		return true
	}
	k.pending.Store(true)
	return false
}

// again reports whether a kick arrived while the running one worked; false releases it.
func (k *queueKicker) again() bool {
	if k.pending.CompareAndSwap(true, false) {
		return true
	}
	k.running.Store(false)
	if k.pending.Load() && k.running.CompareAndSwap(false, true) {
		return true
	}
	return false
}
