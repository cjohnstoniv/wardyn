// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// The record of a workload's launched run (launchClaimed): a run the workload took, one it did not, and one
// whose record answered an error that leaves the outcome unknown; and the namespace binding a custom
// resource's launch must still have when it starts its run and when it is recorded.

// errWorkloadRecordUnknown is recordWorkloadRun's error when the record answered an error that may come from a
// commit that landed and its outcome could not be read back either: whether the workload took the run is not
// known.
var errWorkloadRecordUnknown = errors.New("the outcome of the workload's record of its new run is unknown")

// workloadHoldsReadTimeout bounds the sweep's read of whether one owed run's workload holds it, so a row a
// record still locks holds up none of the other owed ends in the pass.
const workloadHoldsReadTimeout = 5 * time.Second

// recordWorkloadRun records runID, the run a launch under claimed's claim has just created, as the workload's
// run, and returns the run it replaced for the caller to end. binding, set for a custom resource's launch, is the
// namespace binding the launch was authorised under: the record holds only while it exists
// (store.ErrWorkloadBindingGone). Any other error may come from a commit that landed, since the record's
// transaction returns its commit's own error, so the outcome is read back under the workload's row lock, on a
// bounded context the caller's going away does not end. A run the workload took is kept and answers no
// error; the run it replaced was owed its end in the record, so sweepWorkloadRunEnds ends it. So is a run the
// workload took that a later launch has already replaced with a queued run: its end waits on that run, and
// sweepWorkloadRunEnds settles it.
func (s *Server) recordWorkloadRun(r *http.Request, ws store.WorkloadStore, claimed store.Workload, binding *uuid.UUID, runID uuid.UUID) (*uuid.UUID, error) {
	ctx := r.Context()
	var prev *uuid.UUID
	var err error
	if binding != nil {
		prev, err = ws.SetBoundWorkloadRun(ctx, claimed.ID, claimed.ClaimToken, claimed.Generation, runID, *binding)
	} else {
		prev, err = ws.SetWorkloadRun(ctx, claimed.ID, claimed.ClaimToken, claimed.Generation, runID)
	}
	if err == nil || errors.Is(err, store.ErrWorkloadSuperseded) || errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrWorkloadBindingGone) {
		return prev, err
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), killCascadeTimeout)
	defer cancel()
	held, deferred, rerr := ws.WorkloadHoldsRun(rctx, claimed.Owner, claimed.Name, runID)
	switch {
	case rerr != nil:
		return nil, fmt.Errorf("%w: %w; reading it back: %w", errWorkloadRecordUnknown, err, rerr)
	case held, deferred:
		slog.WarnContext(ctx, "wardynd: a workload's record of its new run answered an error but landed", slog.String("run_id", runID.String()),
			slog.String("workload", workloadTarget(claimed)), slog.Any("err", err))
		return nil, nil
	}
	return nil, err
}

// untakenWorkloadLaunch answers a claimed launch whose run, runID, recordWorkloadRun did not make the
// workload's (err), and ends the run: another launch was recorded first, the workload is gone, or the
// namespace binding the launch was authorised under was deleted while the run was starting, which ends it
// workload_binding_deleted and refuses the launch workload_binding_inactive. The current run keeps running. A record whose outcome is unknown
// ends nothing: the run is owed its end, and sweepWorkloadRunEnds ends it unless the workload holds it
// (endOwedWorkloadRunUnlessHeld).
func (s *Server) untakenWorkloadLaunch(r *http.Request, ws store.WorkloadStore, claimed store.Workload, runID uuid.UUID, err error) (client.WorkloadLaunch, error) {
	none := client.WorkloadLaunch{State: client.WorkloadNoLaunch}
	switch {
	case errors.Is(err, errWorkloadRecordUnknown):
		ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), killCascadeTimeout)
		defer cancel()
		e := store.WorkloadRunEnd{RunID: runID, Owner: claimed.Owner, Name: claimed.Name, Generation: claimed.Generation, Reason: workloadKillReplaced}
		if oerr := ws.OweWorkloadRunEnd(ctx, e); oerr != nil {
			slog.ErrorContext(ctx, "wardynd: record a workload's run as owed its end", slog.String("run_id", runID.String()),
				slog.String("workload", workloadTarget(claimed)), slog.Any("err", oerr))
		}
		return none, err
	case errors.Is(err, store.ErrNotFound):
		s.endUntakenWorkloadRun(r, ws, runID, claimed, workloadKillDeleted)
		return none, nil
	case errors.Is(err, store.ErrWorkloadBindingGone):
		s.endUntakenWorkloadRun(r, ws, runID, claimed, workloadKillBindingDeleted)
		return s.refuseWorkloadLaunch(r, ws, claimed, reasonWorkloadBindingInactive, workloadBindingGoneMsg)
	}
	s.endUntakenWorkloadRun(r, ws, runID, claimed, workloadKillReplaced)
	if errors.Is(err, store.ErrWorkloadSuperseded) {
		return none, nil
	}
	return none, err
}

// endOwedWorkloadRunUnlessHeld is the sweep's end of e's run, an owed end that waits on no replacement, unless
// the workload holds the run after all: only a record whose outcome was unknown leaves a run it took owed
// (untakenWorkloadLaunch). The read and the drop are one transaction under the workload's row lock
// (store.WorkloadStore.DropWorkloadRunEndIfHeld), so a record that replaces the run meanwhile keeps the end
// it owes. A run it holds keeps running and is owed nothing; one that cannot be read stays owed for the next
// pass.
func (s *Server) endOwedWorkloadRunUnlessHeld(ctx context.Context, ws store.WorkloadStore, e store.WorkloadRunEnd) {
	hctx, cancel := context.WithTimeout(ctx, workloadHoldsReadTimeout)
	held, err := ws.DropWorkloadRunEndIfHeld(hctx, e.Owner, e.Name, e.RunID)
	cancel()
	switch {
	case err != nil:
		slog.WarnContext(ctx, "wardynd: read whether a workload holds a run owed its end", slog.String("run_id", e.RunID.String()),
			slog.String("workload", e.Owner+"/"+e.Name), slog.Any("err", err))
	case !held:
		s.endOwedWorkloadRun(ctx, e, types.ActorSystem, "wardynd")
	}
}

// bindingStanding reads b again under the launch's claim, then its authority: a launch can wait in the
// background after b was found (a report's launch, the lapsed-launch sweep's retry), and b may be deleted by
// then. found is false once b is deleted; live is bindingLive's answer for a b still found. A delete that
// lands after this read is fenced at the record (store.WorkloadStore.SetBoundWorkloadRun).
func (s *Server) bindingStanding(ctx context.Context, ws store.WorkloadStore, b store.WorkloadBinding, si types.ServiceIdentity) (found, live bool, err error) {
	if _, err = ws.GetWorkloadBinding(ctx, b.ID); errors.Is(err, store.ErrNotFound) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	live, err = s.bindingLive(ctx, b, si)
	return true, live, err
}
