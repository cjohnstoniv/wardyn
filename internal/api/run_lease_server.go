// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"sync"
	"time"
)

// RunLeaseConfig is the run lease's part of Config (run_lease.go), embedded so
// its fields read as Config's own.
type RunLeaseConfig struct {
	// EndedRunGrace mirrors WARDYN_ENDED_RUN_GRACE: how long a run its lease
	// ended keeps its files (stopped, no network) before it is torn down. 0
	// tears a run down at its end.
	EndedRunGrace time.Duration
}

// runLeaseState is the run lease's part of Server, embedded so its fields read
// as Server's own.
type runLeaseState struct {
	// leaseEnded holds the id of every kept run whose broker credentials this
	// process has revoked (run_lease.go), so the lease sweep's re-assert
	// revokes once per process, not every pass. Pruned by the sweep.
	leaseEnded sync.Map
	// containmentFailed holds the id of every kept run whose unresolved
	// containment this process has audited (run_lease.go, #1060), so a stop
	// that keeps failing is audited once per process, not every pass. Cleared
	// on resolution and pruned by the sweep.
	containmentFailed sync.Map
	// reviving holds the id of every run a revive is replacing the proxy of
	// in this process (run_revive.go), so two revives of one run cannot race
	// each other's remove-and-create of the same sidecar.
	reviving sync.Map
	// liveRevived holds when this process last started replacing each live
	// run's proxy (run_revive.go, #1005), so a revive of a live run is bounded
	// to one per reviveLiveEvery. An entry drops itself once it has lapsed.
	liveRevived sync.Map
	// runOps holds each run's operation lock (run_oplock.go, #1480): a
	// *sync.Mutex that the revive and the lease sweep's destructive
	// re-assertion hold across their read and their runner call. Pruned by
	// the sweep.
	runOps sync.Map
}
