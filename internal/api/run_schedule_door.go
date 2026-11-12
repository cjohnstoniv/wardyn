// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/authz"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The deny_scheduled_runs door: a governance profile turns off start times for the people it covers,
// on top of the deployment switch (WARDYN_SCHEDULED_RUNS), which stays the master. The door only
// closes: false inherits the switch, so an unset profile refuses nothing and no install refuses
// more on upgrade. Deny wins down a chain of units, so "only team X may schedule" is the denial on
// each other top-level unit. Operators stand outside governance and are exempt.

const scheduledRunsDeniedMsg = "Your organisation's governance profile doesn't allow scheduled runs. Start the run now, or ask your admin."

// scheduleDoorRefusal is the 403 scheduled_runs_denied for a caller whose profile turns off scheduled
// runs, with the authz.denied row and the lock a governance door carries. nil when the door is open.
// It judges the ceiling only; callers decide whether a start time is in play.
func (s *Server) scheduleDoorRefusal(ctx context.Context, c governanceCeiling) *runRefusal {
	if !c.Limits.DenyScheduledRuns || s.runUngoverned(ctx) {
		return nil
	}
	d := authz.Deny(authz.ReasonGovernanceUnit, "runs.schedule", scheduledRunsDeniedMsg).WithPolicy(s.ceilingPolicy(ctx, c))
	if c.Unit != nil {
		d = d.OnKnob(s.runLockOf(ctx, c.Unit, "deny_scheduled_runs"))
	}
	return runDoorDenied(reasonScheduledRunsDenied, d)
}

// stepScheduleDoor refuses a start time the profile forbids, at create, Review and preview. It runs
// right after stepPlacement, which parses start_at into f.schedule.
func (s *Server) stepScheduleDoor(f *runFold, _ *foldRecorder) bool {
	if f.schedule == nil {
		return true
	}
	return !s.scheduleDoorRefusal(f.r.Context(), f.ceiling).write(s, f.w, f.r)
}

// queueScheduleDoor ends a booked run whose profile has turned off scheduled runs since it was
// booked, at its start. Only a run with a start time is held to the door.
func (s *Server) queueScheduleDoor(st *queuedStart, ceiling governanceCeiling) *queueEnd {
	if st.entry.StartAt == nil || s.scheduleDoorRefusal(st.ctx, ceiling) == nil {
		return nil
	}
	return &queueEnd{token: string(types.RunEndScheduledRunsDenied), hint: types.RunEndScheduledRunsDenied.Sentence()}
}
