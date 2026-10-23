// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/composer"
	"github.com/cjohnstoniv/wardyn/internal/policyref"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// The restriction write surface: POST /governance/restrictions, PUT and DELETE
// /governance/restrictions/{id}, on the security tier (routes.go). A write is a covered governance
// write: held for a second human when WARDYN_GOVERNANCE_SECOND_HUMAN is on, except a create and a
// replacement that is at least as strict as what it replaces (restrictionAtLeastAsStrict). A delete
// is always held: it frees the people it bound.

const (
	govKindRestriction      = types.GovernanceTargetRestriction
	restrictionWriteAction  = "governance.restriction.write"
	restrictionDeleteAction = "governance.restriction.delete"
)

// mountGovernanceRestrictionRoutes registers the restriction routes on the securityOps group.
func (s *Server) mountGovernanceRestrictionRoutes(securityOps chi.Router) {
	securityOps.Post("/governance/restrictions", s.handleCreateRestriction)
	securityOps.Put("/governance/restrictions/{id}", s.handleUpdateRestriction)
	securityOps.Delete("/governance/restrictions/{id}", s.handleDeleteRestriction)
}

// restrictionView is a restriction as GET /governance lists it: the restriction and how many people
// (members) it names.
type restrictionView struct {
	types.Restriction
	People int `json:"people"`
}

func restrictionViews(list []types.Restriction) []restrictionView {
	out := make([]restrictionView, len(list))
	for i, r := range list {
		out[i] = restrictionView{Restriction: r, People: len(r.Members)}
	}
	return out
}

// restrictionState is the part of a restriction a write replaces and an approval compares: its members
// in a fixed order.
func restrictionState(r types.Restriction) types.RestrictionWrite {
	m := slices.Clone(r.Members)
	sortRestrictionMembers(m)
	return types.RestrictionWrite{Name: r.Name, Spec: r.Spec, Limits: r.Limits, Contact: r.Contact, Members: m}
}

func sortRestrictionMembers(m []types.RestrictionMember) {
	slices.SortFunc(m, func(a, b types.RestrictionMember) int {
		return strings.Compare(string(a.SubjectType)+"\x00"+a.Subject, string(b.SubjectType)+"\x00"+b.Subject)
	})
}

// restrictionAsSpec is the knobs a restriction names, as the policy the shared validator judges.
func restrictionAsSpec(sp types.RestrictionSpec) types.RunPolicySpec {
	spec := types.RunPolicySpec{MinConfinementClass: types.CC1}
	if sp.DeniedDomains != nil {
		spec.DeniedDomains = *sp.DeniedDomains
	}
	if sp.FirstUseApproval != nil {
		spec.FirstUseApproval = *sp.FirstUseApproval
	}
	if sp.FirstUseHoldSeconds != nil {
		spec.FirstUseHoldSeconds = *sp.FirstUseHoldSeconds
	}
	if sp.MaxHolds != nil {
		spec.MaxHolds = *sp.MaxHolds
	}
	if sp.MinConfinementClass != nil {
		spec.MinConfinementClass = *sp.MinConfinementClass
	}
	if sp.AutoStopAfterSec != nil {
		spec.AutoStopAfterSec = *sp.AutoStopAfterSec
	}
	return spec
}

// validateRestriction refuses a write a restriction cannot hold, and normalizes it: the name is
// trimmed, an all-empty contact is none, the members are canonical and de-duplicated. A bool is
// refused when true: a restriction only turns a permission off.
func validateRestriction(b *types.RestrictionWrite) error {
	b.Name = strings.TrimSpace(b.Name)
	if b.Name == "" || len(b.Name) > maxGovernanceUnitNameLen {
		return fmt.Errorf("name: required, at most %d characters", maxGovernanceUnitNameLen)
	}
	if err := types.RestrictionBoolsFalseOnly(b.Spec, b.Limits); err != nil {
		return err
	}
	if b.Contact != nil && b.Contact.IsZero() {
		b.Contact = nil
	}
	if b.Contact != nil {
		if err := policyref.Validate(*b.Contact); err != nil {
			return err
		}
	}
	if err := validateAuthoredPolicySpec(restrictionAsSpec(b.Spec)); err != nil {
		return err
	}
	for i, g := range b.Spec.WithheldGrants {
		if _, ok := withheldGrantKinds[g.Kind]; !ok {
			return fmt.Errorf("withheld_grants[%d]: unknown grant kind %q", i, g.Kind)
		}
		if !withheldScopeless(g.Scope) && !validWithheldScope(g) {
			return fmt.Errorf("withheld_grants[%d]: the scope does not decode into a usable pairing", i)
		}
	}
	eff, err := restrictionEffective(b.Spec, b.Limits)
	if err != nil {
		return err
	}
	if msg := governanceLimitsRefusal(eff.Limits); msg != "" {
		return errors.New(msg)
	}
	members := make([]types.RestrictionMember, 0, len(b.Members))
	for _, m := range b.Members {
		a := types.GovernanceMembership{SubjectType: m.SubjectType, Subject: m.Subject, UnitID: uuid.New()}
		if err := validateGovernanceMembership(&a); err != nil {
			return fmt.Errorf("members: %w", err)
		}
		m = types.RestrictionMember{SubjectType: a.SubjectType, Subject: a.Subject}
		if !slices.Contains(members, m) {
			members = append(members, m)
		}
	}
	sortRestrictionMembers(members)
	b.Members = members
	return nil
}

var withheldGrantKinds = map[types.GrantKind]struct{}{
	types.GrantAPIKey: {}, types.GrantGitPAT: {}, types.GrantSSHKey: {}, types.GrantEnvSecret: {},
	types.GrantFileSecret: {}, types.GrantGitHubToken: {}, types.GrantCloudSTS: {},
}

// validWithheldScope reports whether a scoped withheld entry names a pairing that decodes.
func validWithheldScope(g types.GrantSpec) bool {
	_, _, _, _, err := storedSecretGrantPairing(g)
	return err == nil
}

// restrictionEffective is a restriction's spec and limits met into the unbounded authority: what it
// leaves a person, as the order composer.Leq compares.
func restrictionEffective(sp types.RestrictionSpec, l types.RestrictionLimits) (composer.Authority, error) {
	top := composer.Authority{
		Ceiling: types.RunPolicySpec{AllowAllEgress: true},
		Limits: types.GovernanceLimits{LocalSelfDefinedComponents: true,
			RunLimits: types.RunLimits{AllowNoEnd: true, UserChangesLimits: true}},
	}
	out, _, err := composer.ApplyOverlay(top, composer.Overlay{Ceiling: sp.Overlay(), Limits: l.Overlay()})
	return out, err
}

// restrictionAtLeastAsStrict reports whether replacing old by next is exempt from a second human:
// every old deny is kept (composer.Leq over what each leaves a person), the contact is unchanged (a
// contact is who a refused person is told to ask), and next covers every member old does, or everyone.
func restrictionAtLeastAsStrict(old types.Restriction, next types.RestrictionWrite) bool {
	a, err := restrictionEffective(next.Spec, next.Limits)
	if err != nil {
		return false
	}
	b, err := restrictionEffective(old.Spec, old.Limits)
	if err != nil {
		return false
	}
	if !composer.Leq(a.Ceiling, b.Ceiling, a.Limits, b.Limits) || !sameJSON(old.Contact, next.Contact) {
		return false
	}
	for _, g := range old.Spec.WithheldGrants {
		if !grantWithheldBy(g, next.Spec.WithheldGrants) {
			return false
		}
	}
	if slices.Contains(next.Members, types.RestrictionMember{SubjectType: types.CapabilitySubjectAll}) {
		return true
	}
	for _, m := range old.Members {
		if !slices.Contains(next.Members, m) {
			return false
		}
	}
	return true
}

func (s *Server) handleCreateRestriction(w http.ResponseWriter, r *http.Request) {
	s.writeRestriction(w, r, uuid.New(), true)
}

func (s *Server) handleUpdateRestriction(w http.ResponseWriter, r *http.Request) {
	id, ok := parseIDParam(w, r, "id", "restriction")
	if !ok {
		return
	}
	s.writeRestriction(w, r, id, false)
}

// writeRestriction is the create (POST) and the full replace (PUT, members included) of restriction id.
func (s *Server) writeRestriction(w http.ResponseWriter, r *http.Request, id uuid.UUID, create bool) {
	rs, ok := s.restrictionStore()
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRestrictionInvalid, "this store keeps no restrictions")
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var body types.RestrictionWrite
	if msg := decodeStrictMsg(w, r, &body); msg != "" {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRestrictionInvalid, msg)
		return
	}
	if err := validateRestriction(&body); err != nil {
		writeErrorReason(w, http.StatusUnprocessableEntity, reasonRestrictionInvalid, err.Error())
		return
	}
	var cur types.Restriction
	if !create {
		var err error
		if cur, err = rs.GetRestriction(r.Context(), id); notFoundIf(w, err, "restriction", reasonRestrictionNotFound) {
			return
		} else if err != nil {
			writeServerError(w, r, "read restriction", err)
			return
		}
	}
	if mode == govQueue && !create && !restrictionAtLeastAsStrict(cur, body) {
		op, key := "update", id.String()
		s.proposeGovernanceChange(w, r, govProposal{kind: govKindRestriction, op: op, key: key,
			payload: restrictionChangePayload{ID: id, Write: &body},
			before:  restrictionState(cur), after: body, changed: changedPaths(restrictionState(cur), body), baseState: restrictionState(cur)})
		return
	}
	saved, err := rs.PutRestriction(r.Context(), types.Restriction{ID: id, Name: body.Name, Spec: body.Spec, Limits: body.Limits,
		Contact: body.Contact, Members: body.Members}, principalFromRequest(r))
	if errors.Is(err, store.ErrConflict) {
		writeErrorReason(w, http.StatusConflict, reasonRestrictionInvalid, fmt.Sprintf("a restriction named %q already exists", body.Name))
		return
	}
	if err != nil {
		writeServerError(w, r, "write restriction", err)
		return
	}
	var before any
	if !create {
		before = restrictionState(cur)
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		restrictionWriteAction, id.String(), "success", mustJSON(restrictionAuditData(saved, before, body))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindRestriction, id.String(), "success", nil)
	}
	status := http.StatusOK
	if create {
		status = http.StatusCreated
	}
	writeJSON(w, status, saved)
}

func restrictionAuditData(saved types.Restriction, before any, after types.RestrictionWrite) map[string]any {
	return map[string]any{"name": saved.Name, "members": len(saved.Members), "changed": changedPaths(before, after)}
}

// handleDeleteRestriction removes a restriction (204). It is always held under the second-human
// switch: deleting one frees every person it bound.
func (s *Server) handleDeleteRestriction(w http.ResponseWriter, r *http.Request) {
	rs, ok := s.restrictionStore()
	if !ok {
		writeErrorReason(w, http.StatusNotImplemented, reasonRestrictionInvalid, "this store keeps no restrictions")
		return
	}
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "restriction")
	if !ok {
		return
	}
	cur, err := rs.GetRestriction(r.Context(), id)
	if notFoundIf(w, err, "restriction", reasonRestrictionNotFound) {
		return
	} else if err != nil {
		writeServerError(w, r, "read restriction", err)
		return
	}
	if mode == govQueue {
		s.proposeGovernanceChange(w, r, govProposal{kind: govKindRestriction, op: "delete", key: id.String(),
			payload: restrictionChangePayload{ID: id}, before: restrictionState(cur), changed: changedPaths(restrictionState(cur), nil),
			baseState: restrictionState(cur)})
		return
	}
	if _, err := rs.DeleteRestriction(r.Context(), id); err != nil {
		if notFoundIf(w, err, "restriction", reasonRestrictionNotFound) {
			return
		}
		writeServerError(w, r, "delete restriction", err)
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		restrictionDeleteAction, id.String(), "success", mustJSON(map[string]any{"name": cur.Name})))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindRestriction, id.String(), "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// restrictionChangePayload is a held restriction write: Write nil is a delete. A create is never held.
type restrictionChangePayload struct {
	ID    uuid.UUID               `json:"id"`
	Write *types.RestrictionWrite `json:"write,omitempty"`
}

// applyRestrictionChange applies a held restriction update or delete inside the decision transaction:
// the target lock, the restriction as it now stands compared with what the proposal reviewed, then the
// write on q.
func applyRestrictionChange(_ *Server, r *http.Request, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	ctx := r.Context()
	var p restrictionChangePayload
	if err := decodeHeldPayload(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	if err := store.LockGovernanceTarget(ctx, q, govKindRestriction, p.ID.String()); err != nil {
		return govApplied{}, err
	}
	cur, err := store.GetRestrictionQ(ctx, q, p.ID)
	if errors.Is(err, store.ErrNotFound) {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if err != nil {
		return govApplied{}, err
	}
	before := restrictionState(cur)
	if computeETag(before) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if ch.Op == "delete" {
		if _, err := store.DeleteRestrictionQ(ctx, q, p.ID); err != nil {
			return govApplied{}, err
		}
		return govApplied{action: restrictionDeleteAction, target: p.ID.String(), data: map[string]any{"name": cur.Name}}, nil
	}
	if p.Write == nil {
		return govApplied{}, fmt.Errorf("governance change %s: an update with no body", ch.ID)
	}
	held := *p.Write
	if err := validateRestriction(&held); err != nil {
		return govApplied{}, writeRefusal(http.StatusUnprocessableEntity, reasonRestrictionInvalid, "%s", err.Error())
	}
	saved, err := store.PutRestrictionQ(ctx, q, types.Restriction{ID: p.ID, Name: held.Name, Spec: held.Spec, Limits: held.Limits,
		Contact: held.Contact, Members: held.Members}, ch.ProposedBy)
	if errors.Is(err, store.ErrConflict) {
		return govApplied{}, writeRefusal(http.StatusConflict, reasonRestrictionInvalid, "a restriction named %q already exists", held.Name)
	}
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: restrictionWriteAction, target: p.ID.String(), data: restrictionAuditData(saved, before, held)}, nil
}
