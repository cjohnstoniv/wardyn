// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/cjohnstoniv/wardyn/internal/agentpolicy"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Agent rules (#2949): GET /agent-rules, PUT and DELETE /agent-rules/{harness}, on the security tier
// (routes.go). A rule is an organisation's narrow-only walls for one agent harness, met into every run that
// includes it (run_agent_rules.go). A write follows the second-person switch (governanceWriteMode), as the
// organisation's recording level does: with WARDYN_GOVERNANCE_SECOND_HUMAN on, a PUT or DELETE is held for
// a second security admin.

const (
	govKindAgentRules      = types.GovernanceTargetAgentRules
	agentRulesWriteAction  = "agent_rules.write"
	agentRulesDeleteAction = "agent_rules.delete"
)

// mountAgentRulesRoutes registers the agent rules routes on the securityOps group.
func (s *Server) mountAgentRulesRoutes(securityOps chi.Router) {
	securityOps.Get("/agent-rules", s.handleListAgentRules)
	securityOps.Put("/agent-rules/{harness}", s.handlePutAgentRules)
	securityOps.Delete("/agent-rules/{harness}", s.handleDeleteAgentRules)
}

// agentRulesStore is the store's agent rules, or a 501 when this server's store keeps none.
func (s *Server) agentRulesStore(w http.ResponseWriter) (store.AgentRulesStore, bool) {
	rs, ok := s.cfg.Store.(store.AgentRulesStore)
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonAgentRulesUnavailable, "Agent rules are not available on this server yet.")
		return nil, false
	}
	return rs, true
}

// validateAgentRulesWrite holds a write to what a rule may say. The PUT, a held change's apply and the run
// step on a stored row all use it, so a rule a later, stricter rule refuses never runs unchecked.
func validateAgentRulesWrite(harness string, w types.AgentRulesWrite) *runRefusal {
	if _, ok := harnessByID(harness); !ok || harness == noHarnessCLI {
		return runError(http.StatusUnprocessableEntity, reasonAgentRulesHarnessUnknown,
			"There is no agent harness named "+harness+"; agent rules name a harness from the catalogue.")
	}
	if err := validatePersonalWalls(w.Spec); err != nil {
		return runError(http.StatusUnprocessableEntity, reasonAgentRulesInvalid, "Agent rules: "+err.Error())
	}
	if w.Spec.GitHubCapabilities != nil || w.Spec.AzureDevOpsCapabilities != nil {
		return runError(http.StatusUnprocessableEntity, reasonAgentRulesInvalid,
			"Agent rules cannot set github_capabilities or azure_devops_capabilities yet: Wardyn would not enforce them on every run. "+
				"Use push_rules or withheld_grants, or a restriction.")
	}
	if err := validateHarnessInnerWalls(w.InnerWalls); err != nil {
		return runError(http.StatusUnprocessableEntity, reasonAgentRulesInvalid, "Agent rules: "+err.Error())
	}
	if agentpolicy.LoosensInnerWalls(w.InnerWalls) {
		return runError(http.StatusUnprocessableEntity, reasonAgentRulesInvalid,
			"Agent rules only narrow a run: inner_walls cannot turn the harness's sandbox off or its approvals to none.")
	}
	if (types.AgentRules{Spec: w.Spec, InnerWalls: w.InnerWalls}).Empty() {
		return runError(http.StatusUnprocessableEntity, reasonAgentRulesInvalid, "These agent rules name nothing. Delete them instead.")
	}
	return nil
}

// agentRulesStateView is a harness's rules as a held change's reviewer reads them and its approval compares.
type agentRulesStateView struct {
	Harness    string                   `json:"harness"`
	Spec       types.PersonalWallsSpec  `json:"spec"`
	InnerWalls *types.HarnessInnerWalls `json:"inner_walls,omitempty"`
	Version    int                      `json:"version"`
}

// agentRulesState is nil when there is no row; its computeETag is the base hash of a held change.
func agentRulesState(r *types.AgentRules) any {
	if r == nil {
		return nil
	}
	return agentRulesStateView{Harness: r.Harness, Spec: r.Spec, InnerWalls: r.InnerWalls, Version: r.Version}
}

// agentRulesChangePayload is a held write or delete. Version is the delete's expected version; a write
// carries its own in Write.Version.
type agentRulesChangePayload struct {
	Harness string                 `json:"harness"`
	Version int                    `json:"version,omitempty"`
	Write   *types.AgentRulesWrite `json:"write,omitempty"`
}

// agentRulesWriteData is the audit data of a write: the version, spec and inner walls before and after.
// before is nil for a create.
func agentRulesWriteData(before *types.AgentRules, after types.AgentRules) map[string]any {
	data := map[string]any{"before_version": 0, "after_version": after.Version, "before": nil, "after": after.Spec,
		"inner_walls_before": nil, "inner_walls_after": after.InnerWalls}
	if before != nil {
		data["before_version"], data["before"], data["inner_walls_before"] = before.Version, before.Spec, before.InnerWalls
	}
	return data
}

func agentRulesDeleteData(gone types.AgentRules) map[string]any {
	return map[string]any{"version": gone.Version, "spec": gone.Spec, "inner_walls": gone.InnerWalls}
}

// handleListAgentRules answers {"rules": [...]}, never null, sorted by harness.
func (s *Server) handleListAgentRules(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.agentRulesStore(w)
	if !ok {
		return
	}
	list, err := rs.ListAgentRules(r.Context())
	if err != nil {
		writeServerError(w, r, "list agent rules", err)
		return
	}
	if list == nil {
		list = []types.AgentRules{}
	}
	slices.SortFunc(list, func(a, b types.AgentRules) int { return strings.Compare(a.Harness, b.Harness) })
	writeJSON(w, http.StatusOK, map[string]any{"rules": list})
}

// handlePutAgentRules writes a harness's rules: a create at version 0, else a replacement at the stored
// version. Held for a second security admin when the switch is on.
func (s *Server) handlePutAgentRules(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.agentRulesStore(w)
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	harness := chi.URLParam(r, "harness")
	var body types.AgentRulesWrite
	if !decodeStrict(w, r, &body) {
		return
	}
	if refusal := validateAgentRulesWrite(harness, body); refusal != nil {
		writeErrorReason(w, refusal.status, refusal.body.Reason, refusal.body.Error)
		return
	}
	next := types.AgentRules{Harness: harness, Spec: body.Spec, InnerWalls: body.InnerWalls, Version: body.Version + 1}
	if mode == govQueue {
		var cur *types.AgentRules
		err := s.readGovernanceState(r, func(q store.Querier) error {
			row, err := store.GetAgentRulesQ(r.Context(), q, harness)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err == nil {
				cur = &row
			}
			return err
		})
		if err != nil {
			writeServerError(w, r, "read agent rules", err)
			return
		}
		if agentRulesVersionOf(cur) != body.Version {
			writeErrorReason(w, http.StatusConflict, reasonAgentRulesVersionConflict, agentRulesVersionMsg)
			return
		}
		op := "create"
		if cur != nil {
			op = "update"
		}
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindAgentRules, op: op, key: harness, payload: agentRulesChangePayload{Harness: harness, Write: &body},
			before: agentRulesState(cur), after: agentRulesState(&next), changed: changedPaths(agentRulesState(cur), agentRulesState(&next)),
			baseState: agentRulesState(cur),
		})
		return
	}
	cur, err := currentAgentRules(r, rs, harness)
	if err != nil {
		writeServerError(w, r, "read agent rules", err)
		return
	}
	if agentRulesVersionOf(cur) != body.Version {
		writeErrorReason(w, http.StatusConflict, reasonAgentRulesVersionConflict, agentRulesVersionMsg)
		return
	}
	saved, err := rs.PutAgentRules(r.Context(), types.AgentRules{Harness: harness, Spec: body.Spec, InnerWalls: body.InnerWalls,
		UpdatedBy: principalFromRequest(r)}, body.Version)
	if errors.Is(err, store.ErrAgentRulesVersion) {
		writeErrorReason(w, http.StatusConflict, reasonAgentRulesVersionConflict, agentRulesVersionMsg)
		return
	}
	if err != nil {
		writeServerError(w, r, "write agent rules", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		agentRulesWriteAction, harness, "success", mustJSON(agentRulesWriteData(cur, saved))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindAgentRules, harness, "success", nil)
	}
	writeJSON(w, http.StatusOK, saved)
}

const agentRulesVersionMsg = "The agent rules are at another version. Reload them and try again."

func agentRulesVersionOf(r *types.AgentRules) int {
	if r == nil {
		return 0
	}
	return r.Version
}

// currentAgentRules is a harness's stored rules, nil when it has none.
func currentAgentRules(r *http.Request, rs store.AgentRulesStore, harness string) (*types.AgentRules, error) {
	all, err := rs.ListAgentRules(r.Context())
	if err != nil {
		return nil, err
	}
	for _, row := range all {
		if row.Harness == harness {
			return &row, nil
		}
	}
	return nil, nil
}

// handleDeleteAgentRules removes a harness's rules at ?version (204). Held for a second security admin when
// the switch is on.
func (s *Server) handleDeleteAgentRules(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.agentRulesStore(w)
	if !ok {
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	harness := chi.URLParam(r, "harness")
	version, err := strconv.Atoi(r.URL.Query().Get("version"))
	if err != nil || version <= 0 {
		writeErrorReason(w, http.StatusBadRequest, reasonAgentRulesInvalid, "Name the version to delete with ?version=<n>, a positive integer.")
		return
	}
	noRules := "There are no agent rules for " + harness + "."
	if mode == govQueue {
		var cur *types.AgentRules
		err := s.readGovernanceState(r, func(q store.Querier) error {
			row, err := store.GetAgentRulesQ(r.Context(), q, harness)
			if errors.Is(err, store.ErrNotFound) {
				return nil
			}
			if err == nil {
				cur = &row
			}
			return err
		})
		if err != nil {
			writeServerError(w, r, "read agent rules", err)
			return
		}
		if cur == nil {
			writeErrorReason(w, http.StatusNotFound, reasonAgentRulesInvalid, noRules)
			return
		}
		if cur.Version != version {
			writeErrorReason(w, http.StatusConflict, reasonAgentRulesVersionConflict, agentRulesVersionMsg)
			return
		}
		s.proposeGovernanceChange(w, r, govProposal{
			kind: govKindAgentRules, op: "delete", key: harness, payload: agentRulesChangePayload{Harness: harness, Version: cur.Version},
			before: agentRulesState(cur), changed: changedPaths(agentRulesState(cur), nil), baseState: agentRulesState(cur),
		})
		return
	}
	gone, err := rs.DeleteAgentRules(r.Context(), harness, version)
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErrorReason(w, http.StatusNotFound, reasonAgentRulesInvalid, noRules)
		return
	case errors.Is(err, store.ErrAgentRulesVersion):
		writeErrorReason(w, http.StatusConflict, reasonAgentRulesVersionConflict, agentRulesVersionMsg)
		return
	case err != nil:
		writeServerError(w, r, "delete agent rules", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		agentRulesDeleteAction, harness, "success", mustJSON(agentRulesDeleteData(gone))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindAgentRules, harness, "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// applyAgentRulesChange applies a held create, update or delete inside the decision transaction: the target
// lock, the rules as they now stand compared with what the proposal reviewed, then the write on q at the
// stored version.
func applyAgentRulesChange(_ *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p agentRulesChangePayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	del := ch.Op == "delete"
	if !del {
		if p.Write == nil {
			return govApplied{}, fmt.Errorf("governance change %s: a write with no body", ch.ID)
		}
		if refusal := validateAgentRulesWrite(p.Harness, *p.Write); refusal != nil {
			return govApplied{}, writeRefusal(refusal.status, refusal.body.Reason, "%s", refusal.body.Error)
		}
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindAgentRules, p.Harness); err != nil {
		return govApplied{}, err
	}
	var cur *types.AgentRules
	row, err := store.GetAgentRulesQ(ctx, q, p.Harness)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return govApplied{}, err
	default:
		cur = &row
	}
	if (ch.Op == "create") != (cur == nil) || computeETag(agentRulesState(cur)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if del {
		gone, err := store.DeleteAgentRulesQ(ctx, q, p.Harness, cur.Version)
		if err != nil {
			return govApplied{}, err
		}
		return govApplied{action: agentRulesDeleteAction, target: p.Harness, data: agentRulesDeleteData(gone)}, nil
	}
	saved, err := store.PutAgentRulesQ(ctx, q, types.AgentRules{Harness: p.Harness, Spec: p.Write.Spec, InnerWalls: p.Write.InnerWalls,
		UpdatedBy: ch.ProposedBy}, agentRulesVersionOf(cur))
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: agentRulesWriteAction, target: p.Harness, data: agentRulesWriteData(cur, saved)}, nil
}
