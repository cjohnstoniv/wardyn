// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"errors"
	"net/http"
	"slices"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/runnerpool"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A remote-provided pool's use policy narrows who may use it: with none, everyone who may launch remote
// runs may; with one, only the listed people, groups and roles. Written by a security admin or an
// admin (securityOps), and held for a second human under WARDYN_GOVERNANCE_SECOND_HUMAN. Both a set
// and a clear are held, with no narrowing exemption: a clear widens, and nothing proves a replacement
// only narrows. Attaching an executor that another live pool holds under a policy is held too (it puts
// that executor in front of a wider audience), and a pool with a policy is not deleted until the
// policy is cleared. A held change applies in the decision transaction and goes stale when the policy
// or the pool's executors it reviewed have moved.

const govKindRunnerPoolUsePolicy = types.GovernanceTargetRunnerPoolUsePolicy

// runnerPoolUsePolicyRequest is PUT /runner-pools/{id}/use-policy's body. Revision is optional
// optimistic concurrency against the pool revision the caller read.
type runnerPoolUsePolicyRequest struct {
	Revision *int64                    `json:"revision,omitempty"`
	Subjects []types.RunnerPoolSubject `json:"subjects"`
}

// runnerPoolHeldChange is a held change's payload: a use policy set (Subjects), a clear (nothing), an
// executor attach (Executor) or an executor detach (Executor and Detach).
type runnerPoolHeldChange struct {
	Subjects []types.RunnerPoolSubject `json:"subjects,omitempty"`
	Executor string                    `json:"executor,omitempty"`
	Detach   bool                      `json:"detach,omitempty"`
}

// runnerPoolUsePolicyState is the target of a change as it stands, hashed by proposal and approval
// alike: whether a policy exists, its subjects and the executors, so a change goes stale when who may
// use the pool or what it holds moved, and a member adding their own runner elsewhere cannot make one
// stale. Presence is its own field: no policy opens the pool, while a stored empty list (what a leaver
// purge leaves) permits nobody, and both read as no subjects.
func runnerPoolUsePolicyState(present bool, subjects []types.RunnerPoolSubject, executors []string) any {
	return map[string]any{"policy_present": present, "subjects": subjects, "executor_ids": nonNilStrings(executors)}
}

// canonicalPoolSubjects folds each subject to the form the caller's identities are matched in, and
// refuses one that could never match anybody: a non-ASCII group, or a user type that does not exist.
func (s *Server) canonicalPoolSubjects(w http.ResponseWriter, r *http.Request, in []types.RunnerPoolSubject) ([]types.RunnerPoolSubject, bool) {
	out := make([]types.RunnerPoolSubject, 0, len(in))
	for _, sub := range in {
		switch sub.SubjectType {
		case types.CapabilitySubjectUser:
			sub.Subject = types.CanonicalUserSubject(sub.Subject)
		case types.CapabilitySubjectGroup:
			canon, ok := types.CanonicalGroupSubject(sub.Subject)
			if !ok {
				writePoolInvalid(w, "a group subject is printable ASCII, as the sign-in group snapshot carries it")
				return nil, false
			}
			sub.Subject = canon
		case types.CapabilitySubjectUserType:
			if !oidc.UserTypeIDWellFormed(sub.Subject) {
				writePoolInvalid(w, "subject "+sub.Subject+" is not a user type id")
				return nil, false
			}
			if !s.userTypeSubjectExists(w, r, sub.SubjectType, sub.Subject) {
				return nil, false
			}
		}
		out = append(out, sub)
	}
	return out, true
}

// usePolicyPool resolves {id} for a use-policy route: a Remote Provided pool from the caller's
// management catalogue. A Self-Hosted pool has no use policy: its owner is told so in plain words, and
// anyone else's answers exactly as an unknown id. ok=false: the answer is written.
func (s *Server) usePolicyPool(w http.ResponseWriter, r *http.Request, ps store.RunnerPoolStore) (store.RunnerPoolCatalogueRow, bool) {
	row, ok := s.poolFromPath(w, r, ps, false)
	if !ok || row.Pool.HostingType == types.RunnerPoolRemoteProvided {
		return row, ok
	}
	if owner := poolOwner(r.Context()); owner == "" || row.Pool.Owner != owner {
		writePoolNotFound(w)
		return store.RunnerPoolCatalogueRow{}, false
	}
	writeErrorReason(w, runnerpool.ReasonMemberMismatch.Status(), string(runnerpool.ReasonMemberMismatch),
		row.Pool.Name+" is your own Self-Hosted pool: only you use it, so it has no use policy.")
	return store.RunnerPoolCatalogueRow{}, false
}

func (s *Server) handleGetRunnerPoolUsePolicy(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	row, ok := s.usePolicyPool(w, r, ps)
	if !ok {
		return
	}
	// A pool with no policy reads with no subjects and no updated_at. A stored policy always has an
	// updated_at; one whose people all left reads with no subjects too and permits nobody. Revision is
	// the pool's current revision, the one a PUT names.
	pol := types.RunnerPoolUsePolicy{PoolID: row.Pool.ID, Subjects: []types.RunnerPoolSubject{}}
	if row.Policy != nil {
		pol = *row.Policy
	}
	pol.Revision = row.Pool.Revision
	writeJSON(w, http.StatusOK, pol)
}

func (s *Server) handlePutRunnerPoolUsePolicy(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	row, ok := s.usePolicyPool(w, r, ps)
	if !ok {
		return
	}
	var req runnerPoolUsePolicyRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	subjects, ok := s.canonicalPoolSubjects(w, r, req.Subjects)
	if !ok {
		return
	}
	if err := (types.RunnerPoolUsePolicy{Subjects: subjects}).Validate(row.Pool.HostingType); err != nil {
		writePoolInvalid(w, err.Error())
		return
	}
	s.writeUsePolicy(w, r, mode, ps, row, req.Revision, subjects)
}

func (s *Server) handleDeleteRunnerPoolUsePolicy(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	row, ok := s.usePolicyPool(w, r, ps)
	if !ok {
		return
	}
	if row.Policy == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeUsePolicy(w, r, mode, ps, row, nil, nil)
}

// writeUsePolicy carries out a set (subjects non-nil) or a clear (nil) the way a covered write is: held
// for a second human, or applied at once with the audit row (and, for the admin token, the
// break-glass row).
func (s *Server) writeUsePolicy(w http.ResponseWriter, r *http.Request, mode govWriteMode, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow, revision *int64, subjects []types.RunnerPoolSubject) {
	before := []types.RunnerPoolSubject{}
	if row.Policy != nil {
		before = row.Policy.Subjects
	}
	poolID := row.Pool.ID
	if mode == govQueue {
		op := "set"
		if subjects == nil {
			op = "delete"
		}
		after := subjects
		if after == nil {
			after = []types.RunnerPoolSubject{}
		}
		if revision != nil && *revision != row.Pool.Revision {
			writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: row.Pool.Name})
			return
		}
		beforeState := runnerPoolUsePolicyState(row.Policy != nil, before, row.ExecutorIDs)
		afterState := runnerPoolUsePolicyState(subjects != nil, after, row.ExecutorIDs)
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindRunnerPoolUsePolicy, op: op, key: poolID.String(), payload: runnerPoolHeldChange{Subjects: subjects},
			before: beforeState, after: afterState, changed: changedPaths(beforeState, afterState), baseState: beforeState,
		})
		return
	}
	var (
		result types.RunnerPoolUsePolicy
		action = auditPoolUsePolicySet
		err    error
	)
	if subjects == nil {
		action = auditPoolUsePolicyClear
		err = ps.DeleteRunnerPoolUsePolicy(r.Context(), poolID)
	} else {
		result, err = ps.PutRunnerPoolUsePolicy(r.Context(), poolID, revision, subjects, principalFromRequest(r))
	}
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
		return
	}
	if errors.Is(err, store.ErrRunnerPoolStale) {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: row.Pool.Name})
		return
	}
	if err != nil {
		writeServerError(w, r, "write runner pool use policy", err)
		return
	}
	s.auditPool(r, action, poolID.String(), usePolicyAuditData(before, subjects))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindRunnerPoolUsePolicy, poolID.String(), "success", nil)
	}
	if subjects == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func usePolicyAuditData(before, after []types.RunnerPoolSubject) map[string]any {
	if after == nil {
		after = []types.RunnerPoolSubject{}
	}
	return map[string]any{"before": before, "after": after}
}

// holdExecutorChange stores an executor attach or detach as a pending change on the pool, under the
// same kind and the same state hash as its use policy.
func (s *Server) holdExecutorChange(w http.ResponseWriter, r *http.Request, row store.RunnerPoolCatalogueRow, executor string, detach bool) {
	subjects := []types.RunnerPoolSubject{}
	if row.Policy != nil {
		subjects = row.Policy.Subjects
	}
	executors := append(slices.Clone(row.ExecutorIDs), executor)
	if detach {
		executors = slices.DeleteFunc(slices.Clone(row.ExecutorIDs), func(id string) bool { return id == executor })
	}
	before := runnerPoolUsePolicyState(row.Policy != nil, subjects, row.ExecutorIDs)
	after := runnerPoolUsePolicyState(row.Policy != nil, subjects, executors)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindRunnerPoolUsePolicy, op: opExecutorAttach, key: row.Pool.ID.String(), payload: runnerPoolHeldChange{Executor: executor, Detach: detach},
		before: before, after: after, changed: changedPaths(before, after), baseState: before,
	})
}

// opExecutorAttach is the op of an executor attach or detach: migration 0126 closes the set of ops, so
// it is a "set" and the payload (Executor, no Subjects) tells it from a policy set.
const opExecutorAttach = "set"

// applyRunnerPoolUsePolicyChange applies a held use policy set or clear, or executor attach, inside the
// decision transaction: the target lock, then the pool's policy and executors read as they now stand
// and compared with what the proposal reviewed, and the write through the pool row lock.
func applyRunnerPoolUsePolicyChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	poolID, err := uuid.Parse(ch.TargetKey)
	if err != nil {
		return govApplied{}, err
	}
	var held runnerPoolHeldChange
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, err
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindRunnerPoolUsePolicy, poolID.String()); err != nil {
		return govApplied{}, err
	}
	// The pool row next, before anything is read: every member and policy write takes it, so none can
	// commit between the stale check and the apply.
	if err := store.LockLivePoolQ(ctx, q, poolID); errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	} else if err != nil {
		return govApplied{}, err
	}
	before, present := []types.RunnerPoolSubject{}, false
	switch pol, err := store.GetRunnerPoolUsePolicyQ(ctx, q, poolID); {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return govApplied{}, err
	default:
		before, present = pol.Subjects, true
	}
	executors, err := store.RunnerPoolExecutorIDsQ(ctx, q, poolID)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(runnerPoolUsePolicyState(present, before, executors)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if held.Executor != "" {
		if held.Detach {
			return applyExecutorDetach(ctx, q, poolID, held.Executor)
		}
		return s.applyExecutorAttach(ctx, q, poolID, held.Executor)
	}
	action := auditPoolUsePolicySet
	if ch.Op == "delete" {
		action = auditPoolUsePolicyClear
		err = store.DeleteRunnerPoolUsePolicyQ(ctx, q, poolID)
	} else if err = (types.RunnerPoolUsePolicy{Subjects: held.Subjects}).Validate(types.RunnerPoolRemoteProvided); err == nil {
		_, err = store.PutRunnerPoolUsePolicyQ(ctx, q, poolID, nil, held.Subjects, principalFromRequest(r))
	}
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: action, target: poolID.String(), data: usePolicyAuditData(before, held.Subjects)}, nil
}

func (s *Server) applyExecutorAttach(ctx context.Context, q store.Querier, poolID uuid.UUID, executor string) (govApplied, error) {
	if !slices.Contains(s.configuredExecutors(), executor) {
		return govApplied{}, writeRefusal(http.StatusUnprocessableEntity, string(runnerpool.ReasonMemberMismatch), "executor %s is not configured on this server", executor)
	}
	_, _, rev, err := store.AddRunnerPoolExecutorQ(ctx, q, poolID, executor)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: auditPoolExecutorAdd, target: poolID.String(), data: map[string]any{
		"executor_id": executor, "hosting_type": types.RunnerPoolRemoteProvided, "revision": rev}}, nil
}

func applyExecutorDetach(ctx context.Context, q store.Querier, poolID uuid.UUID, executor string) (govApplied, error) {
	_, rev, err := store.RemoveRunnerPoolExecutorQ(ctx, q, poolID, executor)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: auditPoolExecutorRemove, target: poolID.String(), data: map[string]any{
		"executor_id": executor, "hosting_type": types.RunnerPoolRemoteProvided, "revision": rev}}, nil
}
