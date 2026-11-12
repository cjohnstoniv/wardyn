// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// startRunEndLoops starts the loops that own a run after its end: the containment loop, the revoke
// queue and the tail owner. Each runs on every replica on its own ticker and stops with the watcher ctx.
func (s *Server) startRunEndLoops() {
	s.startContainLoop()
	s.startRevokeQueue()
	s.startTailOwner()
}

// runEndState is the process state of the loops that own a run after its end. Each part is its
// owning lane's type, in that lane's file. Zero value is ready to use.
type runEndState struct {
	contain  containState   // the containment loop: in-flight set, streak clocks, stop FIFO (run_contain.go)
	endHolds endHoldSlots   // the end-hold semaphore (run_end_hold.go)
	tail     tailOwnerState // the tail owner's per-run backoff and given_up re-writes (run_tail_owner.go)
}
