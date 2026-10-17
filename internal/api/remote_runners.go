// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"
	"slices"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
	"github.com/cjohnstoniv/wardyn/pkg/client"
)

// Remote-hosted runners: the server's own Docker host or Kubernetes cluster, one row per configured
// substrate (store_remote_runners.go). Operators (admin, security admin) read them all; an admin
// renames one; the "Available to people's pools" policy is written by an admin or a security admin and
// held for a second human under WARDYN_GOVERNANCE_SECOND_HUMAN, as a pool-use policy is. A person reads
// only the ones they may add to their own user pools, and adds one only while its policy admits them.

const (
	auditRemoteRunnerCreate      = "remote_runner.create"
	auditRemoteRunnerUpdate      = "remote_runner.update"
	auditRemoteRunnerPeopleSet   = "remote_runner.people_policy.set"
	auditRemoteRunnerPeopleClear = "remote_runner.people_policy.delete"

	govKindRemoteRunnerPeople = types.GovernanceTargetRemoteRunnerPeople
)

func (s *Server) remoteRunnerStore(w http.ResponseWriter) (store.RemoteRunnerStore, bool) {
	rs, ok := s.cfg.Store.(store.RemoteRunnerStore)
	if !ok {
		writeErrorReason(w, runnerpool.ReasonPoolsUnavailable.Status(), string(runnerpool.ReasonPoolsUnavailable), runnerpool.UnavailableServerMsg())
	}
	return rs, ok
}

// remoteRunnerConfigured reports whether this server runs the substrate a remote runner row names.
func (s *Server) remoteRunnerConfigured(id string) bool {
	return slices.Contains(s.configuredExecutors(), id)
}

// refuseRemoteMember refuses, before any write, a remote-hosted runner id that cannot join pool: one
// this server is not set up with (an unknown id reads the same). ok=true: the refusal is written.
func (s *Server) refuseRemoteMember(w http.ResponseWriter, pool types.RunnerPool, rid string) bool {
	if (types.RunnerPoolMember{PoolID: pool.ID, RemoteRunnerID: rid}).Validate(pool.Scope, "") != nil || !s.remoteRunnerConfigured(rid) {
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), runnerpool.RemoteNotConfiguredMsg(rid))
		return true
	}
	return false
}

// remoteMemberAudit is a remote-hosted member's runner_pool.runner.attach/detach data.
func remoteMemberAudit(pool types.RunnerPool, rid string, rev int64) map[string]any {
	return withPoolOwner(map[string]any{"runner_kind": types.RunnerKindRemoteHosted, "remote_runner_id": rid, "scope": pool.Scope, "revision": rev}, pool)
}

// admitsPeople reports whether a remote runner's people policy admits the caller: never when it is off,
// always when it is everyone, and when it is listed only for a listed subject the caller matches (an
// emptied list admits nobody).
func (c callerSubjects) admitsPeople(rr types.RemoteRunner) bool {
	switch rr.PeoplePools {
	case types.PeoplePoolsEveryone:
		return true
	case types.PeoplePoolsListed:
		return slices.ContainsFunc(rr.PeopleSubjects, c.matchesPoolSubject)
	}
	return false
}

// remoteRunnerViews are rows as the reads show them, each with its usage: every run on it, from every
// pool, since it is shared infrastructure. ok=false: the 500 is written.
func (s *Server) remoteRunnerViews(w http.ResponseWriter, r *http.Request, rows []store.RemoteRunnerRow) ([]client.RemoteRunnerView, bool) {
	ids := make([]string, len(rows))
	for i, row := range rows {
		ids[i] = row.Runner.ID
	}
	used, err := s.runnersInUse(r.Context(), nil, ids)
	if err != nil {
		writeServerError(w, r, "read remote runner usage", err)
		return nil, false
	}
	out := make([]client.RemoteRunnerView, 0, len(rows))
	for _, row := range rows {
		v := client.RemoteRunnerView{RemoteRunner: row.Runner, Configured: s.remoteRunnerConfigured(row.Runner.ID), Pools: row.Pools,
			Usage: s.remoteUsage(r.Context(), row.Runner, used[row.Runner.ID])}
		if v.Pools == nil {
			v.Pools = []types.RemoteRunnerPoolRef{}
		}
		v.Online = v.Usage.Online
		out = append(out, v)
	}
	return out, true
}

// writeRemoteRunner answers one row as the operators' reads show it.
func (s *Server) writeRemoteRunner(w http.ResponseWriter, r *http.Request, row store.RemoteRunnerRow) {
	if views, ok := s.remoteRunnerViews(w, r, []store.RemoteRunnerRow{row}); ok {
		writeJSON(w, http.StatusOK, views[0])
	}
}

func (s *Server) handleListRemoteRunners(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteRunnerStore(w)
	if !ok {
		return
	}
	rows, err := rs.ListRemoteRunners(r.Context())
	if err != nil {
		writeServerError(w, r, "list remote runners", err)
		return
	}
	if out, ok := s.remoteRunnerViews(w, r, rows); ok {
		writeJSON(w, http.StatusOK, out)
	}
}

// remoteRunnerFromPath reads {rid}; ok=false: the 404 or 500 is written.
func (s *Server) remoteRunnerFromPath(w http.ResponseWriter, r *http.Request, rs store.RemoteRunnerStore) (store.RemoteRunnerRow, bool) {
	row, err := rs.GetRemoteRunner(r.Context(), chi.URLParam(r, "rid"))
	if errors.Is(err, store.ErrNotFound) {
		writeErrorReason(w, http.StatusNotFound, reasonRemoteRunnerNotFound, remoteRunnerNotFoundMsg)
		return row, false
	}
	if err != nil {
		writeServerError(w, r, "read remote runner", err)
		return row, false
	}
	return row, true
}

// remoteRunnerNotFoundMsg answers an unknown remote-hosted runner on the operators' routes.
const remoteRunnerNotFoundMsg = "That runner isn't one this server is set up with."

func (s *Server) handleGetRemoteRunner(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteRunnerStore(w)
	if !ok {
		return
	}
	if row, ok := s.remoteRunnerFromPath(w, r, rs); ok {
		s.writeRemoteRunner(w, r, row)
	}
}

// handleUpdateRemoteRunner renames a remote-hosted runner or declares its limits, or both (admin). Its
// limits bound every run on it from every pool, so only an administrator sets them; lowering one ends
// no run, and waiting runs are looked at again at once.
func (s *Server) handleUpdateRemoteRunner(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteRunnerStore(w)
	if !ok {
		return
	}
	var req client.UpdateRemoteRunnerRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if err := req.Validate(); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	cur, ok := s.remoteRunnerFromPath(w, r, rs)
	if !ok {
		return
	}
	_, rr, err := rs.UpdateRemoteRunner(r.Context(), cur.Runner.ID, req.Revision, req.Name, req.Limits, principalFromRequest(r))
	switch {
	case errors.Is(err, store.ErrRunnerPoolStale):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: cur.Runner.Name})
		return
	case err != nil:
		writeServerError(w, r, "update remote runner", err)
		return
	}
	if rr.Revision != cur.Runner.Revision {
		s.auditPool(r, auditRemoteRunnerUpdate, rr.ID, map[string]any{"name_before": cur.Runner.Name, "name": rr.Name,
			"limits_before": cur.Runner.Limits, "limits": rr.Limits, "revision": rr.Revision})
		s.kickQueue()
	}
	cur.Runner = rr
	s.writeRemoteRunner(w, r, cur)
}

// remoteRunnerPeopleChange is a held people-policy write's payload.
type remoteRunnerPeopleChange struct {
	PeoplePools types.RemoteRunnerPeoplePools `json:"people_pools"`
	Subjects    []types.RunnerPoolSubject     `json:"subjects,omitempty"`
}

// remoteRunnerPeopleState is what a people-policy change is reviewed against and hashed by proposal
// and approval alike.
func remoteRunnerPeopleState(rr types.RemoteRunner) any {
	subjects := rr.PeopleSubjects
	if subjects == nil {
		subjects = []types.RunnerPoolSubject{}
	}
	return map[string]any{"people_pools": rr.PeoplePools, "subjects": subjects}
}

func (s *Server) handlePutRemoteRunnerPeoplePolicy(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	rs, ok := s.remoteRunnerStore(w)
	if !ok {
		return
	}
	var req client.RemoteRunnerPeoplePolicyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	subjects := req.Subjects
	switch req.PeoplePools {
	case types.PeoplePoolsEveryone:
		if len(subjects) > 0 {
			writePoolInvalid(w, "Everyone lists no people, groups or roles.")
			return
		}
		subjects = nil
	case types.PeoplePoolsListed:
		if subjects, ok = s.canonicalPoolSubjects(w, r, subjects); !ok {
			return
		}
		if err := types.ValidateRunnerPoolUseSubjects(subjects); err != nil {
			writePoolInvalid(w, err.Error())
			return
		}
	default:
		writePoolInvalid(w, "people_pools is everyone or listed; clear the policy with DELETE.")
		return
	}
	row, ok := s.remoteRunnerFromPath(w, r, rs)
	if !ok {
		return
	}
	s.writeRemoteRunnerPeople(w, r, mode, row.Runner, req.Revision, remoteRunnerPeopleChange{PeoplePools: req.PeoplePools, Subjects: subjects})
}

// handleDeleteRemoteRunnerPeoplePolicy turns the policy off: nobody may add the runner to a user pool.
// It is held like a set, with no narrowing exemption, as a pool-use policy clear is.
func (s *Server) handleDeleteRemoteRunnerPeoplePolicy(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	rs, ok := s.remoteRunnerStore(w)
	if !ok {
		return
	}
	row, ok := s.remoteRunnerFromPath(w, r, rs)
	if !ok {
		return
	}
	if row.Runner.PeoplePools == types.PeoplePoolsOff {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeRemoteRunnerPeople(w, r, mode, row.Runner, nil, remoteRunnerPeopleChange{PeoplePools: types.PeoplePoolsOff})
}

// writeRemoteRunnerPeople carries out a people-policy write the way a covered write is: held for a
// second human, or applied at once under the governance target lock with the audit row.
func (s *Server) writeRemoteRunnerPeople(w http.ResponseWriter, r *http.Request, mode govWriteMode, cur types.RemoteRunner, revision *int64, ch remoteRunnerPeopleChange) {
	if revision != nil && *revision != cur.Revision {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: cur.Name})
		return
	}
	after := cur
	after.PeoplePools, after.PeopleSubjects = ch.PeoplePools, ch.Subjects
	if mode == govQueue {
		op := "set"
		if ch.PeoplePools == types.PeoplePoolsOff {
			op = "delete"
		}
		before, afterState := remoteRunnerPeopleState(cur), remoteRunnerPeopleState(after)
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindRemoteRunnerPeople, op: op, key: cur.ID, payload: ch,
			before: before, after: afterState, changed: changedPaths(before, afterState), baseState: before,
		})
		return
	}
	before, rr, err := s.cfg.Store.(store.RemoteRunnerStore).SetRemoteRunnerPeople(r.Context(), cur.ID, &cur.Revision, ch.PeoplePools, ch.Subjects, principalFromRequest(r))
	switch {
	case errors.Is(err, store.ErrRunnerPoolStale):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: cur.Name})
		return
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonRemoteRunnerNotFound, remoteRunnerNotFoundMsg)
		return
	case err != nil:
		writeServerError(w, r, "write remote runner people policy", err)
		return
	}
	applied := peopleApplied(before, rr)
	s.auditPool(r, applied.action, applied.target, applied.data)
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindRemoteRunnerPeople, cur.ID, "success", nil)
	}
	if ch.PeoplePools == types.PeoplePoolsOff {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, rr)
}

// peopleApplied is a people-policy write's audit row.
func peopleApplied(before, after types.RemoteRunner) govApplied {
	action := auditRemoteRunnerPeopleSet
	if after.PeoplePools == types.PeoplePoolsOff {
		action = auditRemoteRunnerPeopleClear
	}
	return govApplied{action: action, target: after.ID, data: map[string]any{
		"before": remoteRunnerPeopleState(before), "after": remoteRunnerPeopleState(after), "revision": after.Revision}}
}

// applyRemoteRunnerPeopleChange applies a held people-policy write inside the decision transaction:
// the target lock, the runner row as it now stands compared with what the proposal reviewed, and the
// write under the row lock.
func applyRemoteRunnerPeopleChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	var held remoteRunnerPeopleChange
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, err
	}
	if err := store.LockGovernanceTarget(r.Context(), q, govKindRemoteRunnerPeople, ch.TargetKey); err != nil {
		return govApplied{}, err
	}
	cur, err := store.GetRemoteRunnerQ(r.Context(), q, ch.TargetKey, true)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(remoteRunnerPeopleState(cur)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	rr, err := store.SetRemoteRunnerPeopleQ(r.Context(), q, ch.TargetKey, nil, held.PeoplePools, held.Subjects, principalFromRequest(r))
	if err != nil {
		return govApplied{}, err
	}
	return peopleApplied(cur, rr), nil
}

// handleListMyRemoteRunners lists the remote-hosted runners the caller may add to their own user pools:
// configured on this server and admitted by their people policy. Each shows only the caller's own
// pools it is in, and never its people list.
func (s *Server) handleListMyRemoteRunners(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.remoteRunnerStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	subj, err := s.callerSubjects(r.Context())
	if err != nil {
		writeCeilingError(w, r, err)
		return
	}
	rows, err := rs.ListRemoteRunners(r.Context())
	if err != nil {
		writeServerError(w, r, "list remote runners", err)
		return
	}
	rows = slices.DeleteFunc(rows, func(row store.RemoteRunnerRow) bool {
		return !s.remoteRunnerConfigured(row.Runner.ID) || !subj.admitsPeople(row.Runner)
	})
	views, ok := s.remoteRunnerViews(w, r, rows)
	if !ok {
		return
	}
	out := []client.RemoteRunnerView{}
	for _, v := range views {
		v.PeopleSubjects, v.UpdatedBy = nil, ""
		v.Pools = slices.DeleteFunc(v.Pools, func(p types.RemoteRunnerPoolRef) bool { return p.Scope != types.RunnerPoolUser || p.Owner != owner })
		for i := range v.Pools {
			v.Pools[i].Owner = ""
		}
		out = append(out, v)
	}
	writeJSON(w, http.StatusOK, out)
}

// handleAddMyRemoteRunnerToPool puts a remote-hosted runner in the caller's own user pool, only while
// the runner's people policy admits them: the policy is checked here and its revision held under the
// runner row in the insert (AddRemoteRunnerToUserPool), so a narrowing in between refuses the add.
// A runner the caller may not add is answered alike whether it exists or not.
func (s *Server) handleAddMyRemoteRunnerToPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	rs, ok := s.remoteRunnerStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	pool, ok := s.myPool(w, r, ps)
	if !ok {
		return
	}
	rid := chi.URLParam(r, "rid")
	notAvailable := func() {
		writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch), runnerpool.RemoteNotAvailableMsg(rid))
	}
	if (types.RunnerPoolMember{PoolID: pool.ID, RemoteRunnerID: rid}).Validate(pool.Scope, "") != nil || !s.remoteRunnerConfigured(rid) {
		notAvailable()
		return
	}
	row, err := rs.GetRemoteRunner(r.Context(), rid)
	if errors.Is(err, store.ErrNotFound) {
		notAvailable()
		return
	}
	if err != nil {
		writeServerError(w, r, "read remote runner", err)
		return
	}
	subj, err := s.callerSubjects(r.Context())
	if err != nil {
		writeCeilingError(w, r, err)
		return
	}
	if !subj.admitsPeople(row.Runner) {
		notAvailable()
		return
	}
	m, added, rev, err := ps.AddRemoteRunnerToUserPool(r.Context(), pool.ID, rid, owner, row.Runner.Revision)
	switch {
	case errors.Is(err, store.ErrRunnerPoolStale):
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: pool.Name})
		return
	case errors.Is(err, store.ErrNotFound):
		writePoolNotFound(w)
		return
	case err != nil:
		writeServerError(w, r, "add remote runner to pool", err)
		return
	}
	status := http.StatusOK
	if added {
		status = http.StatusCreated
		s.auditPool(r, auditPoolRunnerAdd, pool.ID.String(), remoteMemberAudit(pool, rid, rev))
	}
	writeJSON(w, status, m)
}

// handleRemoveMyRemoteRunnerFromPool takes a remote-hosted runner out of the caller's own user pool,
// whatever its people policy now says.
func (s *Server) handleRemoveMyRemoteRunnerFromPool(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	owner, ok := s.runnerPersonalOwner(w, r)
	if !ok {
		return
	}
	pool, ok := s.myPool(w, r, ps)
	if !ok {
		return
	}
	rid := chi.URLParam(r, "rid")
	removed, rev, err := ps.RemoveRemoteRunnerFromUserPool(r.Context(), pool.ID, rid, owner)
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if err != nil {
		writeServerError(w, r, "remove remote runner from pool", err)
		return
	}
	if removed {
		s.auditPool(r, auditPoolRunnerRemove, pool.ID.String(), remoteMemberAudit(pool, rid, rev))
	}
	w.WriteHeader(http.StatusNoContent)
}
