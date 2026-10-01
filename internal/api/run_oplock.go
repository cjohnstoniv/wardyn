// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"sync"

	"github.com/google/uuid"
)

// The per-run operation lock (#1480). Two paths in this daemon act on one run's
// proxy and broker credentials from a row they read earlier: a revive replaces
// the proxy it just claimed the run for, and the lease sweep re-asserts the
// stop of a kept run it listed before that claim. Neither check-then-act is
// atomic on its own, so a stale sweep could stop the proxy a revive had just
// started. Both hold this run's lock across the read and the runner call, and
// the sweep re-reads the row once it holds it.
//
// Wardyn runs one daemon, so an in-process lock is the whole story; it is per
// run, never global, so two runs never wait on each other. Only the revive
// (reviveRunProxy) and the two sweeps that re-assert a destructive stop
// (leaseRun, keepRebootedRun) take it. It is NOT taken inside loseRun,
// stopLostSandbox, endSandbox, reconcileFinalize or stopKeptRun: they run
// under it, and the mutex is not reentrant. The sweep side only ever
// tries: a run whose lock is held is skipped and retried next pass.

// lockRunOp takes run id's lock, waiting for it, and returns its release.
func (s *Server) lockRunOp(id uuid.UUID) func() {
	for {
		m := s.runOpMutex(id)
		m.Lock()
		if s.runOpCurrent(id, m) {
			return m.Unlock
		}
		m.Unlock() // dropped while this call waited: take the new one
	}
}

// tryLockRunOp is lockRunOp that gives up instead of waiting.
func (s *Server) tryLockRunOp(id uuid.UUID) (func(), bool) {
	for {
		m := s.runOpMutex(id)
		if !m.TryLock() {
			return nil, false
		}
		if s.runOpCurrent(id, m) {
			return m.Unlock, true
		}
		m.Unlock()
	}
}

func (s *Server) runOpMutex(id uuid.UUID) *sync.Mutex {
	if m, ok := s.runOps.Load(id); ok {
		return m.(*sync.Mutex)
	}
	m, _ := s.runOps.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

func (s *Server) runOpCurrent(id uuid.UUID, m *sync.Mutex) bool {
	cur, ok := s.runOps.Load(id)
	return ok && cur == any(m)
}

// dropRunOp forgets run id's lock when nobody holds it, so the map does not
// grow with every run that ever revived. A lock held right now stays: its
// holder, or the next sweep, drops it later.
func (s *Server) dropRunOp(id uuid.UUID) {
	m, ok := s.runOps.Load(id)
	if !ok {
		return
	}
	if mu := m.(*sync.Mutex); mu.TryLock() {
		s.runOps.Delete(id)
		mu.Unlock()
	}
}
