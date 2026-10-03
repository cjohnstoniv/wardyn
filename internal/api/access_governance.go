// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Role mappings under four-eyes (gov4-b2): the gates a role-mapping write passes before the store,
// shared by a direct write and an approval, and the held-change halves of POST and DELETE
// /access/mappings. A role mapping is written on the operatorOnly tier, so only a super admin
// proposes one and only a super admin approves one (operatorApprover); a security admin is refused
// admin_surface and never sees one in the change list.
//
// The lockout guard runs only where a write is applied, against the claim snapshot of the human whose
// request applies it: on an approval that is the approver, never the proposer, who may have been
// demoted since and whom the second human exists to check.
package api

import (
	"errors"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// roleMappingRefusal writes the response of a refused role-mapping write.
type roleMappingRefusal func(w http.ResponseWriter)

// roleMappingUpsertGate is what a role-mapping upsert that passed its gates would write.
type roleMappingUpsertGate struct {
	write     oidc.RoleMapping
	existing  []types.RoleMapping
	candidate []oidc.RoleMapping
}

// gateRoleMappingUpsert runs the gates a role-mapping upsert passes before the store, in the order a
// direct write does: the target's shape, the collision with the chart, the email-mapping opt-in, the
// posture-flip acknowledgement and (when checkLockout) the lockout guard against r's own snapshot.
// listExisting is called after the cheap gates, as a direct write reads the table. A refusal is the
// response to write; err is a store failure.
func (s *Server) gateRoleMappingUpsert(r *http.Request, req roleMappingWriteRequest, value string, userTypes []types.UserType,
	listExisting func() ([]types.RoleMapping, error), checkLockout bool) (g roleMappingUpsertGate, refuse roleMappingRefusal, err error) {
	write, msg := accessMappingTarget(req, value, userTypes)
	if msg != "" {
		return g, func(w http.ResponseWriter) {
			writeErrorReason(w, http.StatusBadRequest, reasonAccessMappingTargetInvalid, msg)
		}, nil
	}
	g.write = write
	if cause := accessCollisionCause(value, s.cfg.OIDC.ChartRoleMap(), s.cfg.OIDC); cause != "" {
		return g, func(w http.ResponseWriter) { writeAccessCollision(w, value, cause) }, nil
	}
	// Adjudication (docs/design/people-access-prompt.md): an email-shaped CONSOLE value is refused
	// unless the org opted in (WARDYN_OIDC_ALLOW_EMAIL_MAPPINGS). Checked AFTER the collision check on
	// purpose: an email that already collides with the chart or the operator allowlist gets that more
	// specific refusal.
	if strings.Contains(value, "@") && !s.cfg.AllowEmailMappings {
		return g, func(w http.ResponseWriter) {
			writeErrorReason(w, http.StatusBadRequest, reasonAccessEmailMappingDisabled, accessEmailKeyRefused)
		}, nil
	}
	if g.existing, err = listExisting(); err != nil {
		return g, nil, err
	}
	g.candidate = accessCandidateRows(g.existing, "", write)
	return g, s.gateRoleMappingEffect(r, req.AcknowledgeAccessChange, g.existing, g.candidate, userTypes, checkLockout), nil
}

// gateRoleMappingEffect is the two guards on what a role-mapping write DOES: the posture-flip
// guard, which fires iff the write actually moves the unmatched-human outcome (derived from the real
// merged map, not a row count) and the caller has not acknowledged it, then the lockout guard.
func (s *Server) gateRoleMappingEffect(r *http.Request, acknowledged bool, existing []types.RoleMapping, candidate []oidc.RoleMapping,
	userTypes []types.UserType, checkLockout bool) roleMappingRefusal {
	if !acknowledged {
		before := s.accessUnmatchedOutcome(toOIDCRoleMappings(existing), userTypes)
		after := s.accessUnmatchedOutcome(candidate, userTypes)
		if before != after {
			return func(w http.ResponseWriter) { writeAccessPostureFlip(w, before, after) }
		}
	}
	if checkLockout {
		if lerr := s.accessLockoutErr(r, existing, candidate, userTypes); lerr != nil {
			return func(w http.ResponseWriter) {
				writeErrorReason(w, http.StatusBadRequest, reasonAccessLockout, lerr.Error())
			}
		}
	}
	return nil
}

// roleMappingState is the target of a role-mapping change as it stands: the row at the value, nil
// when none. A proposal and its approval hash one and the same thing.
func roleMappingState(rows []types.RoleMapping, value string) any {
	for _, m := range rows {
		if m.Value == value {
			return map[string]any{"mapping": m}
		}
	}
	return map[string]any{"mapping": nil}
}

// roleMappingDiffView is a role mapping as the reviewer reads it.
type roleMappingDiffView struct {
	Value    string `json:"value"`
	Role     string `json:"role"`
	UserType string `json:"user_type,omitempty"`
}

func roleMappingViewOf(rows []types.RoleMapping, value string) *roleMappingDiffView {
	for _, m := range rows {
		if m.Value == value {
			return &roleMappingDiffView{Value: m.Value, Role: m.Role, UserType: m.UserType}
		}
	}
	return nil
}

// roleMappingDeletePayload is a held role-mapping delete: the id it resolved from, and whether the
// caller acknowledged the posture flip it may cause (re-evaluated when it applies).
type roleMappingDeletePayload struct {
	ID                      uuid.UUID `json:"id"`
	AcknowledgeAccessChange bool      `json:"acknowledge_access_change,omitempty"`
}

// holdRoleMappingUpsert stores a role-mapping upsert that passed the proposal gates (all but the
// lockout guard) as a pending change.
func (s *Server) holdRoleMappingUpsert(w http.ResponseWriter, r *http.Request, req roleMappingWriteRequest, g roleMappingUpsertGate) {
	value := g.write.Value
	req.Value = value
	after := &roleMappingDiffView{Value: value, Role: g.write.Role, UserType: g.write.UserType}
	before := roleMappingViewOf(g.existing, value)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindRoleMapping, op: "upsert", key: value, payload: req,
		before: before, after: after, changed: changedPaths(before, after), baseState: roleMappingState(g.existing, value),
	})
}

// holdRoleMappingDelete stores a role-mapping delete as a pending change. A delete by id is resolved
// to the value here, so a pending delete and a pending upsert of one value collide.
func (s *Server) holdRoleMappingDelete(w http.ResponseWriter, r *http.Request, id uuid.UUID, acknowledged bool, existing []types.RoleMapping, matched types.RoleMapping) {
	if matched.ID == uuid.Nil {
		writeErrorReason(w, http.StatusNotFound, reasonRoleMappingNotFound, "role mapping not found")
		return
	}
	before := roleMappingViewOf(existing, matched.Value)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindRoleMapping, op: "delete", key: matched.Value,
		payload: roleMappingDeletePayload{ID: id, AcknowledgeAccessChange: acknowledged},
		before:  before, changed: changedPaths(before, nil), baseState: roleMappingState(existing, matched.Value),
	})
}

// refusalOf is a role-mapping refusal carried out of the decision transaction, so it rolls back and the
// change stays pending.
func refusalOf(why string, refuse roleMappingRefusal) *govRefusal {
	return &govRefusal{why: why, write: func(w http.ResponseWriter, _ *http.Request) { refuse(w) }}
}

// applyRoleMappingChange applies a held role-mapping change inside the decision transaction. Every
// gate a direct write passes is run again against the table as it stands, including the lockout guard
// against the APPROVER's snapshot (r is the approver's request).
func applyRoleMappingChange(s *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	if s.cfg.OIDC == nil {
		return govApplied{}, refusalOf("sso not configured", func(w http.ResponseWriter) {
			writeErrorReason(w, http.StatusServiceUnavailable, reasonSSONotConfigured, "SSO is not configured")
		})
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindRoleMapping, ch.TargetKey); err != nil {
		return govApplied{}, err
	}
	existing, err := store.ListRoleMappingsQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	userTypes, err := store.ListUserTypesQ(ctx, q)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(roleMappingState(existing, ch.TargetKey)) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if ch.Op == "delete" {
		return s.applyRoleMappingDelete(r, q, ch, existing, userTypes)
	}
	var req roleMappingWriteRequest
	if err := decodeHeldPayload(ch.Payload, &req); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonAccessMappingTargetInvalid, "invalid role mapping: %v", err)
	}
	value, err := canonicalRoleMapValue(req.Value)
	if err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonAccessRoleMapValueInvalid, "%s", err.Error())
	}
	g, refuse, err := s.gateRoleMappingUpsert(r, req, value, userTypes,
		func() ([]types.RoleMapping, error) { return existing, nil }, true)
	if err != nil {
		return govApplied{}, err
	}
	if refuse != nil {
		return govApplied{}, refusalOf("role mapping gate", refuse)
	}
	m := types.RoleMapping{ID: uuid.New(), Value: value, Role: g.write.Role, UserType: g.write.UserType, CreatedBy: ch.ProposedBy}
	saved, err := store.UpsertRoleMappingQ(ctx, q, m)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonAccessUnknownUserType, "%s", accessUnknownUserType(g.write.UserType))
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{
		action: "access.role_mapping.write", target: saved.ID.String(),
		data: map[string]any{"value": saved.Value, "role": saved.Role, "user_type": saved.UserType},
		afterCommit: func() map[string]any {
			return s.demotionAuditData(r, saved.Value, "upsert", existing, g.candidate, userTypes)
		},
	}, nil
}

func (s *Server) applyRoleMappingDelete(r *http.Request, q store.Querier, ch types.GovernanceChange, existing []types.RoleMapping, userTypes []types.UserType) (govApplied, error) {
	var p roleMappingDeletePayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, writeRefusal(http.StatusBadRequest, reasonInvalidRequestBody, "invalid role mapping delete: %v", err)
	}
	var matched types.RoleMapping
	for _, m := range existing {
		if m.ID == p.ID {
			matched = m
		}
	}
	candidate := accessCandidateRows(existing, p.ID.String(), oidc.RoleMapping{})
	if refuse := s.gateRoleMappingEffect(r, p.AcknowledgeAccessChange, existing, candidate, userTypes, true); refuse != nil {
		return govApplied{}, refusalOf("role mapping gate", refuse)
	}
	if err := store.DeleteRoleMappingQ(r.Context(), q, p.ID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return govApplied{}, writeRefusal(http.StatusNotFound, reasonRoleMappingNotFound, "role mapping not found")
		}
		return govApplied{}, err
	}
	return govApplied{
		action: "access.role_mapping.delete", target: p.ID.String(),
		data: map[string]any{"value": matched.Value, "role": matched.Role, "user_type": matched.UserType},
		afterCommit: func() map[string]any {
			return s.demotionAuditData(r, matched.Value, "delete", existing, candidate, userTypes)
		},
	}, nil
}

// demotionAuditData is the part of a role-mapping write that follows its commit: a demotion made here
// is effective here, so the tokens that lose a tier are revoked now. The data is what the direct
// write's audit row carries beside the mapping: how many frozen snapshots named the value, how many
// tokens the write revoked, and whether the revocation failed.
func (s *Server) demotionAuditData(r *http.Request, value, what string, before []types.RoleMapping, after []oidc.RoleMapping, userTypes []types.UserType) map[string]any {
	stale := s.noteStaleRoleSnapshots(r.Context(), value, what)
	revoked, revokeErr := s.revokeDemotedRoleSnapshots(r, value, toOIDCRoleMappings(before), after, userTypes)
	data := map[string]any{"stale_token_snapshots": stale, "tokens_revoked": revoked}
	if revokeErr != nil {
		data["tokens_revocation_failed"] = true
	}
	return data
}
