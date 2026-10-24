// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import "sync"

// PauseFence orders a guest's pause and resume requests by their generation (AgentSignal), the run's
// count of sign-in pauses that wardynd keeps: one older than the newest it has seen is dropped, so a
// continue delivered after a later pause's stop never runs the agent under that pause. A request with
// no generation (0, from a runner or wardynd that predates it) is let through as before and moves
// nothing. The newest generation lives in the agent's memory: a guest agent that starts again starts at
// 0, with the stopped agent it signalled gone with it, and the next pause's generation is still higher.
type PauseFence struct {
	mu   sync.Mutex
	last int64
}

// Do runs signal, under the fence, unless gen is older than the newest generation seen; applied is
// false when it was dropped.
func (f *PauseFence) Do(gen int64, signal func() error) (applied bool, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if gen > 0 {
		if gen < f.last {
			return false, nil
		}
		f.last = gen
	}
	return true, signal()
}
