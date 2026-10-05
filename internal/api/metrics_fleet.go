// Copyright 2025 The Wardyn Authors
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
	"time"

	"github.com/cjohnstoniv/wardyn/internal/store"
)

// fleetSnapshotTTL is how long one capacity aggregate serves every scrape on this replica.
const fleetSnapshotTTL = 15 * time.Second

// fleetSnapshot caches the run-capacity aggregate. The mutex is held across the refresh, so
// concurrent scrapes share one query (single-flight). A failed refresh is not cached.
// Zero value is ready to use.
type fleetSnapshot struct {
	mu   sync.Mutex
	at   time.Time
	data store.RunCapacity
}

// get returns the snapshot, refreshing it when older than the TTL. ok is false when the store
// cannot report capacity or the refresh failed.
func (f *fleetSnapshot) get(ctx context.Context, reader runCapacityReader, kind string) (store.RunCapacity, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.at.IsZero() && time.Since(f.at) < fleetSnapshotTTL {
		return f.data, true
	}
	c, err := reader.RunCapacity(ctx, store.RunCapacityOpts{
		Now: time.Now().UTC(), CurrentKind: kind, UnschedulableReason: capacityBlockerReason,
	})
	if err != nil {
		f.at = time.Time{}
		return store.RunCapacity{}, false
	}
	f.at, f.data = time.Now(), c
	return c, true
}

// writeFleetGauges appends the fleet capacity families. On a refresh failure they are omitted,
// never written as zero: a dead store must not scrape as an empty cluster (see handleMetrics's
// store_up). There is no owner label on any family.
func (s *Server) writeFleetGauges(r *http.Request, w io.Writer) {
	reader, ok := s.cfg.Store.(runCapacityReader)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), storePingTimeout)
	defer cancel()
	c, ok := s.fleet.get(ctx, reader, s.cfg.RunnerTarget)
	if !ok {
		return
	}
	fmt.Fprint(w, "# HELP wardyn_runs_active Non-terminal runs by state, from a 15-second snapshot. Aggregate across replicas with max(), never sum().\n"+
		"# TYPE wardyn_runs_active gauge\n")
	for _, st := range slices.Sorted(maps.Keys(c.States)) {
		fmt.Fprintf(w, "wardyn_runs_active{state=%q} %d\n", st, c.States[st])
	}
	fmt.Fprintf(w, "# HELP wardyn_runs_unschedulable Starting runs waiting for capacity on the runner.\n"+
		"# TYPE wardyn_runs_unschedulable gauge\nwardyn_runs_unschedulable %d\n", c.UnschedulableTotal)
	runners := slices.Sorted(maps.Keys(c.ByRunner))
	fmt.Fprint(w, "# HELP wardyn_runs_cpu_millis_held Configured CPU held by active runs per runner kind (Kubernetes requests, Docker caps). Configured reservations, not observed use.\n"+
		"# TYPE wardyn_runs_cpu_millis_held gauge\n")
	for _, k := range runners {
		fmt.Fprintf(w, "wardyn_runs_cpu_millis_held{runner=%q} %d\n", k, c.ByRunner[k].HeldCPUMillis)
	}
	fmt.Fprint(w, "# HELP wardyn_runs_memory_mib_held Configured memory held by active runs per runner kind (Kubernetes requests, Docker caps). Configured reservations, not observed use.\n"+
		"# TYPE wardyn_runs_memory_mib_held gauge\n")
	for _, k := range runners {
		fmt.Fprintf(w, "wardyn_runs_memory_mib_held{runner=%q} %d\n", k, c.ByRunner[k].HeldMemoryMiB)
	}
	fmt.Fprintf(w, "# HELP wardyn_runs_oldest_active_seconds Age of the oldest active run.\n"+
		"# TYPE wardyn_runs_oldest_active_seconds gauge\nwardyn_runs_oldest_active_seconds %d\n", c.OldestActiveSeconds)
}
