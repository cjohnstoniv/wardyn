// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// ownerPoolRefusal re-asserts, for a revive, what a launch asks of a run's runner pool: the pool's use
// policy still admits the run's owner, and for a remote-hosted runner in the owner's own pool, the
// runner's people policy still admits them. Create and the queue ask both of the caller; a revive asks
// them of the owner, as the other owner re-checks do, with the same sentences.
//
// The owner's own session, when its group snapshot is present and complete, matches on its real groups.
// Anyone else knows the owner only by the sub and the user type stamped on the run, and an owner whose
// snapshot is missing or truncated matches nothing by group, as a launch reads it (callerSubjects.permits):
// only a user row naming their sub or a row for their stamped type admits them. A group is never assumed.
// A pool that is no longer in the owner's catalogue (deleted) admits nobody.
func (s *Server) ownerPoolRefusal(ctx context.Context, run types.AgentRun) (*ownerRefusal, error) {
	ps, ok := s.cfg.Store.(store.RunnerPoolStore)
	if run.RunnerPoolID == nil || run.OperatorOwned || !ok {
		return nil, nil
	}
	subj, owner, err := s.revivePoolSubjects(ctx, run)
	if err != nil {
		return nil, err
	}
	rows, err := ps.ListRunnerPoolCatalogue(ctx, owner, false)
	if err != nil {
		return nil, err
	}
	if rowsNameUnit(rows) {
		// The run's frozen unit, not the owner's now: a revive restores the run that was launched.
		subj.units = s.unitLineage(ctx, run.UnitID)
	}
	row, found := catalogueRow(rows, *run.RunnerPoolID)
	if !found || !subj.permits(row, owner) {
		return &ownerRefusal{status: runnerpool.ReasonNotFound.Status(), reason: string(runnerpool.ReasonNotFound), msg: runnerpool.NotFoundMsg()}, nil
	}
	rs, ok := s.cfg.Store.(store.RemoteRunnerStore)
	if run.RunnerPoolExecutor == "" || row.Pool.Scope != types.RunnerPoolUser || !ok {
		return nil, nil
	}
	rr, err := rs.GetRemoteRunner(ctx, run.RunnerPoolExecutor)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}
	if err != nil || !subj.admitsPeople(rr.Runner) {
		return &ownerRefusal{status: runnerpool.ReasonMemberMismatch.Status(), reason: string(runnerpool.ReasonMemberMismatch),
			msg: runnerpool.RemoteNotAvailableMsg(run.RunnerPoolExecutor)}, nil
	}
	return nil, nil
}

// revivePoolSubjects is the run owner's subjects and pool owner id for the pool checks: the caller's own
// session when the caller is the owner and it can answer in full, else what the run row records of them,
// with no groups. A user type deleted since sign-in leaves the session unable to answer, so the row
// reading decides, as for anyone else.
func (s *Server) revivePoolSubjects(ctx context.Context, run types.AgentRun) (callerSubjects, string, error) {
	if sub := oidcHumanFromContext(ctx); sub != "" && sub == run.CreatedBy && !oidcGroupsTruncatedFromContext(ctx) {
		c, err := s.callerSubjects(ctx)
		if err != nil && !errors.Is(err, errUserTypeUnknown) {
			return callerSubjects{}, "", err
		}
		if err == nil && !c.stale {
			return c, poolOwner(ctx), nil
		}
	}
	return callerSubjects{users: []string{types.CanonicalUserSubject(run.CreatedBy)}, userType: run.UserType}, run.CreatedBy, nil
}
