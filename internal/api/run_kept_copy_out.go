// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"

	"github.com/cjohnstoniv/wardyn/internal/audit"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The copy-out door's own refusals (keptCopyOutRefusal), each answered 409 attach_run_kept. Only the run's owner,
// on a run the door is for, ever reads them.
const (
	keptCopyOutUnreadable   = "Wardyn could not check whether this run's agent is still running, so it cannot be opened to copy work out. Try again."
	keptCopyOutPastEnd      = "This run has reached its end, so work can no longer be copied out of it."
	keptCopyOutAgentStopped = "This run's agent has stopped, so there is nothing running to copy work out of."
	keptCopyOutRemoved      = "This run's runner could not confirm that its network was cut, so it removed the run's sandbox. There is nothing left to copy out."
	keptCopyOutNotCut       = "Wardyn has not confirmed that this run's network was cut, so it cannot be opened to copy work out yet. Try again in a minute."
)

// keptCopyOutRefusal decides whether principal may open run, which is kept (runIsKept), to copy work out: "" when
// the door opens, else the refusal. It opens only for the run's own owner, with no delegation, on a RUNNING run a
// Kubernetes runner lost to an outage, whose agent still runs (agent_stopped_at unset and no containment pending
// report), whose proxy stop the server holds confirmed (containment_error empty), before the run's end and its
// lifetime cap on the database's clock. Any other caller or run gets ended, the door's own kept refusal, so nobody
// else learns more than before. A read that fails refuses.
func (s *Server) keptCopyOutRefusal(ctx context.Context, run types.AgentRun, principal, ended string) string {
	if run.LostAt == nil || run.LostReason != types.LostOutage || run.State != types.RunRunning || run.SandboxRef == "" {
		return ended
	}
	if principal == "" || run.CreatedBy == "" || principal != run.CreatedBy || run.OperatorOwned {
		return ended
	}
	if _, delegated := audit.DelegationFrom(ctx); delegated {
		return ended
	}
	if run.Placement != types.PlacementLocal || run.RunnerID == nil || !s.runOnKubernetes(run) {
		return ended
	}
	stops, ok := s.cfg.Store.(runAgentStoppedReader)
	if !ok {
		return ended
	}
	if _, ok := s.cfg.Store.(runContainmentStore); !ok {
		return ended
	}
	now, err := s.dbNow(ctx)
	if err != nil {
		return keptCopyOutUnreadable
	}
	if (run.EndsAt != nil && !now.Before(*run.EndsAt)) || pastLifetimeCap(run, now) {
		return keptCopyOutPastEnd
	}
	// The server's last stop of this run's proxy was not confirmed (settleContainment): the door waits for the
	// lease sweep's next confirmed stop, which clears containment_error.
	if run.ContainmentError != "" {
		return keptCopyOutNotCut
	}
	switch stopped, err := stops.RunAgentStopped(ctx, run.ID); {
	case err != nil:
		return keptCopyOutUnreadable
	case stopped:
		return keptCopyOutAgentStopped
	}
	switch held, err := s.runContainmentPending(ctx, run); {
	case err != nil:
		return keptCopyOutUnreadable
	case held:
		return keptCopyOutRemoved
	}
	return ""
}

// keptCopyOutOpen is whether principal may copy work out of run now: GET /runs/{id}'s kept_copy_out.
func (s *Server) keptCopyOutOpen(ctx context.Context, run types.AgentRun, principal string) bool {
	return runIsKept(run) && s.keptCopyOutRefusal(ctx, run, principal, "-") == ""
}
