// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// PUT /runners/{id}/trust: a security operator's change of a runner's trust class, the only writer after
// the claim. With WARDYN_GOVERNANCE_SECOND_HUMAN on, a human's change is held for a second human (the
// runner_trust governance kind) and the admin token's is break-glass. Nothing the runner sends, its
// registration and posture included, reaches this column.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/placement"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

const govKindRunnerTrust = types.GovernanceTargetRunnerTrust

// runnerTrustStore is the PG capability behind the trust write.
type runnerTrustStore interface {
	GetRunner(ctx context.Context, id uuid.UUID) (types.Runner, error)
	SetRunnerTrustClass(ctx context.Context, id uuid.UUID, class types.RunnerTrustClass, by string, now time.Time) (before, after types.Runner, err error)
}

var _ runnerTrustStore = store.PG{}

// runnerTrustChange is the validated request a held change replays.
type runnerTrustChange struct {
	RunnerID   uuid.UUID              `json:"runner_id"`
	TrustClass types.RunnerTrustClass `json:"trust_class"`
	Reason     string                 `json:"reason"`
}

// runnerTrustDiffView is the trust class as the reviewer reads it.
type runnerTrustDiffView struct {
	TrustClass types.RunnerTrustClass `json:"trust_class"`
}

// runnerTrustState is the target of a trust change as it stands; a proposal and its approval hash it.
func runnerTrustState(row types.Runner) any {
	return map[string]any{"state": row.State, "owner_kind": row.OwnerKind, "trust_class": row.TrustClass}
}

// runnerTrustRefusal says why row cannot take class, or "" when it can.
func runnerTrustRefusal(row types.Runner, class types.RunnerTrustClass) (int, string) {
	switch {
	case row.State != types.RunnerClaimed:
		return http.StatusConflict, "claim the runner first: its claim sets the initial trust class"
	case row.OwnerKind == types.RunnerOwnerOrg && !class.FitsOwner(row.OwnerKind):
		return http.StatusUnprocessableEntity, "an organization's runner is always org_cluster"
	case !class.FitsOwner(row.OwnerKind):
		return http.StatusUnprocessableEntity, "org_cluster is the class of an organization's runner only; a person's runner is personal or managed_device"
	}
	return 0, ""
}

func (s *Server) handlePutRunnerTrust(w http.ResponseWriter, r *http.Request) {
	st, ok := s.cfg.Store.(runnerTrustStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRunnerRegistrationUnavailable, "runner trust requires the Postgres store backend")
		return
	}
	id, ok := parseIDParam(w, r, "id", "runner")
	if !ok {
		return
	}
	var req types.RunnerTrustRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if !req.TrustClass.Valid() {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerTrustInvalid, "trust_class must be personal, managed_device or org_cluster")
		return
	}
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || utf8.RuneCountInString(reason) > maxGovernanceRejectReasonLen || !controlCharFree(reason) {
		writeErrorReason(w, http.StatusBadRequest, reasonRunnerTrustInvalid, "reason is required: at most 512 characters, without control characters")
		return
	}
	row, err := st.GetRunner(r.Context(), id)
	if err == nil && (row.State == types.RunnerRevoked || runnerLapsed(row, s.cfg.Now().UTC())) {
		err = store.ErrNotFound
	}
	if notFoundIf(w, err, "runner", reasonRunnerNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "read runner", err)
		return
	}
	if code, msg := runnerTrustRefusal(row, req.TrustClass); msg != "" {
		writeErrorReason(w, code, reasonRunnerTrustInvalid, msg)
		return
	}
	if row.TrustClass == req.TrustClass {
		s.answerRunnerTrust(w, r, row)
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	change := runnerTrustChange{RunnerID: id, TrustClass: req.TrustClass, Reason: reason}
	if mode == govQueue {
		before, after := &runnerTrustDiffView{TrustClass: row.TrustClass}, &runnerTrustDiffView{TrustClass: req.TrustClass}
		s.proposeGovernanceChange(w, r, govProposal{kind: govKindRunnerTrust, op: "update", key: id.String(), payload: change,
			before: before, after: after, changed: changedPaths(before, after), baseState: runnerTrustState(row)})
		return
	}
	prev, row, err := st.SetRunnerTrustClass(r.Context(), id, req.TrustClass, principalFromRequest(r), s.cfg.Now().UTC())
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrRunnerTrustRefused) {
		writeErrorReason(w, http.StatusConflict, reasonRunnerTrustInvalid, "the runner changed while the trust class was written; read it again")
		return
	}
	if err != nil {
		writeServerError(w, r, "write runner trust class", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r), placement.ActionRunnerTrustWrite, id.String(), "success",
		mustJSON(runnerTrustAuditData(prev.TrustClass, row.TrustClass, reason))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindRunnerTrust, id.String(), "success", nil)
	}
	s.answerRunnerTrust(w, r, row)
}

func runnerTrustAuditData(before, after types.RunnerTrustClass, reason string) map[string]any {
	return map[string]any{"before": before, "after": after, "reason": reason}
}

// answerRunnerTrust answers the runner as the administrators' routes show it.
func (s *Server) answerRunnerTrust(w http.ResponseWriter, r *http.Request, row types.Runner) {
	rs, ok := s.runnerInventoryStore(w)
	if !ok {
		return
	}
	if views, ok := s.runnerViews(w, r, rs, []types.Runner{row}, true); ok {
		writeJSON(w, http.StatusOK, views[0])
	}
}

// applyRunnerTrustChange applies a held trust change inside the decision transaction: the runner row
// locked, compared with what the proposal reviewed, then written through the store's Querier form. The
// proposer is recorded as the writer, since the class is theirs; the decision rows name the approver.
func applyRunnerTrustChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var c runnerTrustChange
	if err := decodeHeldPayload(ch.Payload, &c); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	row, err := store.GetRunnerForUpdateQ(ctx, q, c.RunnerID)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(runnerTrustState(row)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	after, err := store.SetRunnerTrustClassQ(ctx, q, c.RunnerID, c.TrustClass, ch.ProposedBy, s.cfg.Now().UTC())
	if errors.Is(err, store.ErrRunnerTrustRefused) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: placement.ActionRunnerTrustWrite, target: c.RunnerID.String(),
		data: runnerTrustAuditData(row.TrustClass, after.TrustClass, c.Reason)}, nil
}
