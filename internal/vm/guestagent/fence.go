// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

// PauseFence orders a guest's pause and resume requests (AgentSignal) by their generation, the run's
// count of sign-in pauses that wardynd keeps, and within one generation puts the stop before the
// continue. So a continue delivered after a later pause's stop never runs the agent under that pause,
// and a stop delivered again after its own pause was resumed never stops it once that mark is cleared.
// A stop wardynd sends again for a pause still marked takes a generation of its own.
//
// The rules, for a request of generation gen:
//   - older than Generation: dropped;
//   - equal: a continue, or a stop again before the continue, acts; a stop after the continue is
//     dropped;
//   - newer: acts, and becomes the newest;
//   - 0 (none, from a runner or wardynd that predates it): acts as before while no generation has been
//     seen, and moves nothing; once one has, it is dropped.
//
// The guest agent keeps it in its marker directory, a tmpfs, written before each signal: a restart of
// the agent's service keeps it, as the processes it signalled may outlive that restart, and the guest's
// boot clears it with them.
type PauseFence struct {
	Generation int64 `json:"generation"`
	// Continued says Generation's continue has acted.
	Continued bool `json:"continued"`
}

// Admit says whether a stop (cont false) or continue of generation gen may act, and moves f to it
// when it may. The caller holds one lock across Admit and the signal, so nothing acts between them.
func (f *PauseFence) Admit(gen int64, cont bool) bool {
	switch {
	case gen == 0:
		return f.Generation == 0
	case gen < f.Generation:
		return false
	case gen == f.Generation:
		if f.Continued && !cont {
			return false
		}
		f.Continued = f.Continued || cont
	default:
		f.Generation, f.Continued = gen, cont
	}
	return true
}
