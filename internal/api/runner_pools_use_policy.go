// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"net/http"

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
// only narrows. A held change applies in the decision transaction and goes stale when the policy it
// reviewed has moved.

const govKindRunnerPoolUsePolicy = types.GovernanceTargetRunnerPoolUsePolicy

// runnerPoolUsePolicyRequest is PUT /runner-pools/{id}/use-policy's body. Revision is optional
// optimistic concurrency against the pool revision the caller read.
type runnerPoolUsePolicyRequest struct {
	Revision *int64                    `json:"revision,omitempty"`
	Subjects []types.RunnerPoolSubject `json:"subjects"`
}

// runnerPoolUsePolicyState is the target of a change as it stands, hashed by proposal and approval
// alike: the subjects only, so a member adding their own runner elsewhere cannot make a change stale.
func runnerPoolUsePolicyState(subjects []types.RunnerPoolSubject) any {
	return map[string]any{"subjects": subjects}
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

func (s *Server) handleGetRunnerPoolUsePolicy(w http.ResponseWriter, r *http.Request) {
	ps, ok := s.runnerPoolStore(w)
	if !ok {
		return
	}
	row, ok := s.poolFromPath(w, r, ps, false)
	if !ok {
		return
	}
	pol := types.RunnerPoolUsePolicy{PoolID: row.Pool.ID, Subjects: []types.RunnerPoolSubject{}, Revision: row.Pool.Revision}
	if row.Policy != nil {
		pol = *row.Policy
	}
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
	row, ok := s.poolFromPath(w, r, ps, false)
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
		if row.Pool.HostingType != types.RunnerPoolRemoteProvided {
			writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonMemberMismatch, Name: row.Pool.Name, Hosting: row.Pool.HostingType})
			return
		}
		writePoolInvalid(w, err.Error())
		return
	}
	if req.Revision != nil && *req.Revision != row.Pool.Revision {
		writePoolRefusal(w, runnerpool.Refusal{Reason: runnerpool.ReasonStale, Name: row.Pool.Name})
		return
	}
	s.writeUsePolicy(w, r, mode, ps, row, subjects)
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
	row, ok := s.poolFromPath(w, r, ps, false)
	if !ok {
		return
	}
	if row.Policy == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	s.writeUsePolicy(w, r, mode, ps, row, nil)
}

// writeUsePolicy carries out a set (subjects non-nil) or a clear (nil) the way a covered write is: held
// for a second human, or applied at once with the audit row (and, for the admin token, the
// break-glass row).
func (s *Server) writeUsePolicy(w http.ResponseWriter, r *http.Request, mode govWriteMode, ps store.RunnerPoolStore, row store.RunnerPoolCatalogueRow, subjects []types.RunnerPoolSubject) {
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
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindRunnerPoolUsePolicy, op: op, key: poolID.String(), payload: runnerPoolUsePolicyRequest{Subjects: subjects},
			before: runnerPoolUsePolicyState(before), after: runnerPoolUsePolicyState(after),
			changed:   changedPaths(runnerPoolUsePolicyState(before), runnerPoolUsePolicyState(after)),
			baseState: runnerPoolUsePolicyState(policySubjects(row.Policy)),
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
		result, err = ps.PutRunnerPoolUsePolicy(r.Context(), poolID, subjects, principalFromRequest(r))
	}
	if errors.Is(err, store.ErrNotFound) {
		writePoolNotFound(w)
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

// policySubjects is a policy's subjects, empty for none: the shape both hashes of a change read.
func policySubjects(p *types.RunnerPoolUsePolicy) []types.RunnerPoolSubject {
	if p == nil {
		return []types.RunnerPoolSubject{}
	}
	return p.Subjects
}

func usePolicyAuditData(before, after []types.RunnerPoolSubject) map[string]any {
	if after == nil {
		after = []types.RunnerPoolSubject{}
	}
	return map[string]any{"before": before, "after": after}
}

// applyRunnerPoolUsePolicyChange applies a held set or clear inside the decision transaction: the
// target lock, then the pool row, the policy read as it now stands and compared with what the
// proposal reviewed, and the pool re-checked as a live remote-provided one.
func applyRunnerPoolUsePolicyChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	poolID, err := uuid.Parse(ch.TargetKey)
	if err != nil {
		return govApplied{}, err
	}
	var held runnerPoolUsePolicyRequest
	if err := decodeHeldPayload(ch.Payload, &held); err != nil {
		return govApplied{}, err
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindRunnerPoolUsePolicy, poolID.String()); err != nil {
		return govApplied{}, err
	}
	var before []types.RunnerPoolSubject
	switch pol, err := store.GetRunnerPoolUsePolicyQ(ctx, q, poolID); {
	case errors.Is(err, store.ErrNotFound):
		before = []types.RunnerPoolSubject{}
	case err != nil:
		return govApplied{}, err
	default:
		before = pol.Subjects
	}
	if computeETag(runnerPoolUsePolicyState(before)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	action := auditPoolUsePolicySet
	if ch.Op == "delete" {
		action = auditPoolUsePolicyClear
		err = store.DeleteRunnerPoolUsePolicyQ(ctx, q, poolID)
	} else if err = (types.RunnerPoolUsePolicy{Subjects: held.Subjects}).Validate(types.RunnerPoolRemoteProvided); err == nil {
		_, err = store.PutRunnerPoolUsePolicyQ(ctx, q, poolID, held.Subjects, principalFromRequest(r))
	}
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: action, target: poolID.String(), data: usePolicyAuditData(before, held.Subjects)}, nil
}
