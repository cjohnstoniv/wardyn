// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance memberships: the writes binding a subject to a unit.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
	"github.com/cjohnstoniv/wardyn/internal/store"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// assignment writes

// governanceMembershipRequest is POST /governance/memberships's body. The
// natural key (unit_id, subject_type, subject) is what a caller names; the row's id and
// provenance are server-assigned. Home marks the unit a person runs in when a run names none.
type governanceMembershipRequest struct {
	SubjectType types.CapabilitySubjectType `json:"subject_type"`
	Subject     string                      `json:"subject"`
	UnitID      uuid.UUID                   `json:"unit_id"`
	Priority    int                         `json:"priority"`
	Home        bool                        `json:"home,omitempty"`
}

// validateGovernanceMembership normalizes a in place and validates it, applying
// the SAME field hygiene validateCapabilityGrant applies to a capability grant
// subject — trim, length, no control characters, and for a GROUP subject the
// shared types.CanonicalGroupSubject the login-time snapshot itself uses —
// because these two tables are written against the identical subject vocabulary
// and are resolved through the identical capabilitySubjects call. A subject
// normalized one way here and another way there is a row that silently never
// matches.
func validateGovernanceMembership(a *types.GovernanceMembership) error {
	if !a.SubjectType.Valid() {
		return fmt.Errorf("subject_type: invalid %q", a.SubjectType)
	}
	if a.UnitID == uuid.Nil {
		return fmt.Errorf("unit_id: required")
	}
	if a.Priority < math.MinInt32 || a.Priority > math.MaxInt32 { // the column is INT
		return fmt.Errorf("priority: out of range")
	}
	if a.SubjectType == types.CapabilitySubjectAll {
		// "all" names every signed-in human; the migration is explicit that
		// subject is '' for this type, so a caller-supplied value is dropped
		// rather than trusted — there is nothing for it to legitimately name,
		// and honouring it would create a second, unreachable 'all' row.
		a.Subject = ""
		return nil
	}
	a.Subject = strings.TrimSpace(a.Subject)
	if a.Subject == "" {
		return fmt.Errorf("subject: required for subject_type %q", a.SubjectType)
	}
	if a.SubjectType == types.CapabilitySubjectGroup {
		// The group half of that hygiene is types.CanonicalGroupSubject, not a
		// lowercase — see validateCapabilityGrant. A group-tier assignment is
		// the sharper case of the two: HasGroupTierMemberships counts the dead
		// row as "a group tier exists", so a subject no snapshot can carry both
		// fails to wall the member it names AND refuses every caller with an
		// unanswerable snapshot on account of an assignment that could never
		// have applied to them.
		subject, ok := types.CanonicalGroupSubject(a.Subject)
		if !ok {
			return fmt.Errorf("subject: must be printable ASCII — a group subject is matched against the login-time group snapshot, which carries printable ASCII only, so this value can never match anyone")
		}
		a.Subject = subject
	} else if a.SubjectType == types.CapabilitySubjectUserType {
		// Verbatim, as validateCapabilityGrant keeps it: a type id is never
		// folded, and existence is the handler's check.
		if !oidc.UserTypeIDWellFormed(a.Subject) {
			return fmt.Errorf("subject: %q is not a user type id", a.Subject)
		}
	} else {
		// The USER half is types.CanonicalUserSubject for the same guard-before-fold
		// reason (capabilities.go): a bare ToLower folds U+212A onto ASCII 'k'
		// and U+0130 onto 'i', so a crafted spelling of a real human's address
		// was stored as THAT human's subject — an assignment binding someone
		// else's ceiling to them.
		a.Subject = types.CanonicalUserSubject(a.Subject)
	}
	if len(a.Subject) > maxCapabilityGrantFieldLen || !controlCharFree(a.Subject) {
		return fmt.Errorf("subject: invalid")
	}
	return nil
}

// handleUpsertGovernanceMembership makes one subject a member of one unit, keyed on
// the natural (unit_id, subject_type, subject): a subject may be a member of several
// units, and re-posting one updates its priority and home. 201 for a genuinely new
// membership, 200 when an existing one was updated — the same created/updated
// signal handleUpsertCapabilityGrant gives, derived the same way (the store
// returns the EXISTING row's id on a conflict, never the candidate's).
// operatorOnly (routes.go).
func (s *Server) handleUpsertGovernanceMembership(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	var req governanceMembershipRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	a := types.GovernanceMembership{
		SubjectType: req.SubjectType,
		Subject:     req.Subject,
		UnitID:      req.UnitID,
		Priority:    req.Priority,
		Home:        req.Home,
	}
	if err := validateGovernanceMembership(&a); err != nil {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceMembershipInvalid, "invalid assignment: "+err.Error())
		return
	}
	if writeMembershipTreeError(w, r, "", defaultMembershipRefusal(a.UnitID, defaultTakesNoMembershipsMsg)) {
		return
	}
	if !s.userTypeSubjectExists(w, r, a.SubjectType, a.Subject) {
		return
	}
	a.ID = uuid.New()
	a.CreatedBy = principalFromRequest(r)
	if mode == govQueue {
		// Every assignment change is held: a repoint can widen its subjects, and nothing here is
		// proven narrowing.
		s.holdMembershipUpsert(w, r, a)
		return
	}
	saved, err := s.cfg.Store.UpsertGovernanceMembership(r.Context(), a)
	// ErrNotFound here is the FK refusing an unknown unit_id — a 404 naming
	// the profile, not a 500, and not a silent no-op.
	if notFoundIf(w, err, "governance profile", reasonGovernanceUnitNotFoundByID) {
		return
	}
	if err != nil {
		writeServerError(w, r, "upsert governance assignment", err)
		return
	}
	status := http.StatusCreated
	if saved.ID != a.ID {
		status = http.StatusOK
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.write", saved.ID.String(), "success", mustJSON(membershipWriteAuditData(saved))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindMembership, saved.ID.String(), "success", nil)
	}
	writeJSON(w, status, saved)
}

// handleDeleteGovernanceMembership removes one assignment by id (204), 404 when
// unknown. This is the SUPPORTED way to widen a principal back to the
// deployment ceiling, which is why it is audited on its own line rather than
// riding a profile delete. operatorOnly (routes.go).
func (s *Server) handleDeleteGovernanceMembership(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "governance assignment")
	if !ok {
		return
	}
	if mode == govQueue {
		// Deleting an assignment widens its subjects back to the deployment ceiling: never exempt.
		s.holdMembershipDelete(w, r, id)
		return
	}
	// The audit names the row the delete removed, not one read beside it.
	removed, err := s.cfg.Store.DeleteGovernanceMembership(r.Context(), id)
	if notFoundIf(w, err, "governance assignment", reasonGovernanceMembershipNotFound) {
		return
	}
	if err != nil {
		writeServerError(w, r, "delete governance assignment", err)
		return
	}
	data := membershipWriteAuditData(removed)
	if activates := s.activationsAfterDelete(r.Context(), removed); len(activates) > 0 {
		data["activates"] = activates
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.delete", id.String(), "success", mustJSON(data)))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindMembership, id.String(), "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}

// membership moves and promotions (the policy tree)

// governanceMembershipMoveRequest is PUT /governance/memberships/{id}'s body: the unit to move the
// membership to.
type governanceMembershipMoveRequest struct {
	UnitID uuid.UUID `json:"unit_id"`
}

// membershipTreePayload is a held move (the membership and its new unit) or promotion (the
// membership alone).
type membershipTreePayload struct {
	ID     uuid.UUID `json:"id"`
	UnitID uuid.UUID `json:"unit_id,omitzero"`
}

const (
	defaultHasNoMembershipsMsg = "Default has no memberships; delete this membership instead"
	// defaultTakesNoMembershipsMsg answers a membership added to the Default unit.
	defaultTakesNoMembershipsMsg = "Default has no memberships: a person no membership places already runs there"
	membershipAlreadyMemberMsg   = "the subject is already a member of that unit; delete this membership instead"
	membershipAlreadyContext     = "this membership is already a context"
)

// errGovernanceTxUnsupported is a store with no committed governance transaction (a test double).
var errGovernanceTxUnsupported = errors.New("api: the store cannot run a governance write transaction")

// writeGovernance runs fn on one committed transaction of the store.
func (s *Server) writeGovernance(ctx context.Context, fn func(q store.Querier) error) error {
	tx, ok := s.cfg.Store.(interface {
		WriteGovernance(context.Context, func(store.Querier) error) error
	})
	if !ok {
		return errGovernanceTxUnsupported
	}
	return tx.WriteGovernance(ctx, fn)
}

// writeMembershipTreeError answers a failed move or promotion and reports true, or reports false for a
// nil error.
func writeMembershipTreeError(w http.ResponseWriter, r *http.Request, what string, err error) bool {
	var refusal *unitWriteError
	switch {
	case err == nil:
		return false
	case errors.As(err, &refusal):
		writeErrorReason(w, refusal.status, refusal.reason, refusal.msg)
	default:
		writeServerError(w, r, what, err)
	}
	return true
}

// handleMoveGovernanceMembership moves one membership to another unit (200 with the moved row). It
// follows the second-human switch exactly as an assignment write does. securityOps (routes.go).
func (s *Server) handleMoveGovernanceMembership(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "governance assignment")
	if !ok {
		return
	}
	var req governanceMembershipMoveRequest
	if !decodeStrict(w, r, &req) {
		return
	}
	if req.UnitID == uuid.Nil {
		writeErrorReason(w, http.StatusBadRequest, reasonGovernanceMembershipInvalid, "invalid assignment: unit_id: required")
		return
	}
	if writeMembershipTreeError(w, r, "", defaultMembershipRefusal(req.UnitID, defaultHasNoMembershipsMsg)) {
		return
	}
	if mode == govQueue {
		s.holdMembershipMove(w, r, membershipTreePayload{ID: id, UnitID: req.UnitID})
		return
	}
	var from, moved types.GovernanceMembership
	err := s.writeGovernance(r.Context(), func(q store.Querier) (err error) {
		from, moved, err = moveMembershipQ(r.Context(), q, id, req.UnitID)
		return err
	})
	if writeMembershipTreeError(w, r, "move governance assignment", err) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.write", moved.ID.String(), "success", mustJSON(membershipMoveAuditData(from, moved))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindMembership, moved.ID.String(), "success", nil)
	}
	writeJSON(w, http.StatusOK, moved)
}

// membershipMoveAuditData is a move's governance.membership.write data: the row as moved, and the unit
// it left.
func membershipMoveAuditData(from, moved types.GovernanceMembership) map[string]any {
	data := membershipWriteAuditData(moved)
	data["from_unit_id"] = from.UnitID
	return data
}

// defaultMembershipRefusal is the 409 for a membership in unitID when unitID is the Default unit, else
// an untyped nil: Default is where a person no membership places runs, so a membership in it means
// nothing. A create, a change, a move, a held proposal of one, its approval and a dry run all ask it.
func defaultMembershipRefusal(unitID uuid.UUID, msg string) error {
	if unitID != store.DefaultUnitID {
		return nil
	}
	return writeRefusal(http.StatusConflict, reasonGovernanceMembershipDefaultUnit, "%s", msg)
}

// membershipNotFound is the refusal for a membership id no row holds.
func membershipNotFound() *unitWriteError {
	return writeRefusal(http.StatusNotFound, reasonGovernanceMembershipNotFound, "governance assignment not found")
}

// moveMembershipQ moves membership id to unitID on q, under the locks an assignment write takes, and
// returns the row before and after. The direct write, the held change's approval and the dry run all
// call it. Refusals are *unitWriteError.
func moveMembershipQ(ctx context.Context, q store.Querier, id, unitID uuid.UUID) (from, moved types.GovernanceMembership, err error) {
	if refusal := defaultMembershipRefusal(unitID, defaultHasNoMembershipsMsg); refusal != nil {
		return from, moved, refusal
	}
	if from, err = lockedMembershipQ(ctx, q, id); err != nil {
		return from, moved, err
	}
	if _, err = store.GetGovernanceUnitQ(ctx, q, unitID, false); errors.Is(err, store.ErrNotFound) {
		return from, moved, writeRefusal(http.StatusNotFound, reasonGovernanceUnitNotFoundByID, "governance unit not found")
	} else if err != nil {
		return from, moved, err
	}
	moved, err = store.MoveGovernanceMembershipQ(ctx, q, id, unitID)
	if errors.Is(err, store.ErrConflict) {
		return from, moved, writeRefusal(http.StatusConflict, reasonGovernanceMembershipInvalid, "%s", membershipAlreadyMemberMsg)
	}
	return from, moved, err
}

// lockedMembershipQ takes the governance graph lock, reads membership id for update, and takes its
// natural key's lock: the order an assignment write takes them in.
func lockedMembershipQ(ctx context.Context, q store.Querier, id uuid.UUID) (types.GovernanceMembership, error) {
	if err := store.LockGovernanceGraph(ctx, q); err != nil {
		return types.GovernanceMembership{}, err
	}
	row, err := store.GetGovernanceMembershipQ(ctx, q, id, true)
	if errors.Is(err, store.ErrNotFound) {
		return row, membershipNotFound()
	}
	if err != nil {
		return row, err
	}
	return row, store.LockGovernanceMembershipKey(ctx, q, string(row.SubjectType), row.Subject)
}

// promoteMembershipQ makes legacy membership id a context on q. A membership that already is one is a
// 409.
func promoteMembershipQ(ctx context.Context, q store.Querier, id uuid.UUID) (types.GovernanceMembership, error) {
	row, err := lockedMembershipQ(ctx, q, id)
	if err != nil {
		return row, err
	}
	if !row.Legacy {
		return row, writeRefusal(http.StatusConflict, reasonGovernanceMembershipInvalid, "%s", membershipAlreadyContext)
	}
	return store.PromoteGovernanceMembershipQ(ctx, q, id)
}

// handlePromoteGovernanceMembership makes a legacy membership a context (200 with the row): the person
// may then run in its unit beside the others. It follows the second-human switch. securityOps
// (routes.go).
func (s *Server) handlePromoteGovernanceMembership(w http.ResponseWriter, r *http.Request) {
	mode, ok := s.governanceWriteMode(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "governance assignment")
	if !ok {
		return
	}
	if mode == govQueue {
		s.holdMembershipPromote(w, r, id)
		return
	}
	var promoted types.GovernanceMembership
	err := s.writeGovernance(r.Context(), func(q store.Querier) (err error) {
		promoted, err = promoteMembershipQ(r.Context(), q, id)
		return err
	})
	if writeMembershipTreeError(w, r, "promote governance assignment", err) {
		return
	}
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.promote", promoted.ID.String(), "success", mustJSON(membershipWriteAuditData(promoted))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindMembership, promoted.ID.String(), "success", nil)
	}
	writeJSON(w, http.StatusOK, promoted)
}

// membershipTreeState is the target of a held move or promotion: the membership, the chain of the
// unit it is in, and for a move the chain of the unit it moves to.
type membershipTreeState struct {
	row       types.GovernanceMembership
	fromChain []types.GovernanceUnit
	toChain   []types.GovernanceUnit
}

// state is what the change's base_hash covers: the row and both chains, so an edit to either unit
// after proposal makes the change stale.
func (st membershipTreeState) state() any {
	return map[string]any{"membership": st.row, "from_chain": st.fromChain, "to_chain": st.toChain}
}

// readMembershipTreeState reads p's membership and the chains of its unit and of p.UnitID (none for a
// promotion).
func readMembershipTreeState(ctx context.Context, q store.Querier, p membershipTreePayload, forUpdate bool) (membershipTreeState, error) {
	var st membershipTreeState
	var err error
	if st.row, err = store.GetGovernanceMembershipQ(ctx, q, p.ID, forUpdate); errors.Is(err, store.ErrNotFound) {
		return st, membershipNotFound()
	} else if err != nil {
		return st, err
	}
	if st.fromChain, err = unitChainQ(ctx, q, st.row.UnitID, forUpdate); err != nil {
		return st, err
	}
	if p.UnitID != uuid.Nil {
		st.toChain, err = unitChainQ(ctx, q, p.UnitID, forUpdate)
	}
	return st, err
}

// holdMembershipMove stores a move as a pending change, after a dry run of the move has shown the
// store would accept it.
func (s *Server) holdMembershipMove(w http.ResponseWriter, r *http.Request, p membershipTreePayload) {
	ctx := r.Context()
	var st membershipTreeState
	var moved types.GovernanceMembership
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (err error) {
		if st, err = readMembershipTreeState(ctx, q, p, false); err != nil {
			return err
		}
		_, moved, err = moveMembershipQ(ctx, q, p.ID, p.UnitID)
		return err
	})
	if writeMembershipTreeError(w, r, "hold governance assignment move", err) {
		return
	}
	before, after := s.newMembershipDiffView(st.row, st.fromChain), s.newMembershipDiffView(moved, st.toChain)
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindMembership, op: "move", key: membershipKey(st.row.UnitID, st.row.SubjectType, st.row.Subject),
		payload: p, before: before, after: after, changed: changedPaths(before, after), baseState: st.state(),
	})
}

// membershipLegacyView is a membership as a promotion's reviewer reads it: the assignment view and
// whether it is a legacy row (one assignment per person) or a context.
type membershipLegacyView struct {
	*membershipDiffView
	Legacy bool `json:"legacy"`
}

// holdMembershipPromote stores a promotion as a pending change.
func (s *Server) holdMembershipPromote(w http.ResponseWriter, r *http.Request, id uuid.UUID) {
	ctx := r.Context()
	p := membershipTreePayload{ID: id}
	var st membershipTreeState
	var promoted types.GovernanceMembership
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (err error) {
		if st, err = readMembershipTreeState(ctx, q, p, false); err != nil {
			return err
		}
		promoted, err = promoteMembershipQ(ctx, q, id)
		return err
	})
	if writeMembershipTreeError(w, r, "hold governance assignment promotion", err) {
		return
	}
	before := membershipLegacyView{s.newMembershipDiffView(st.row, st.fromChain), st.row.Legacy}
	after := membershipLegacyView{s.newMembershipDiffView(promoted, st.fromChain), promoted.Legacy}
	s.proposeGovernanceChange(w, r, govProposal{
		kind: govKindMembership, op: "promote", key: membershipKey(st.row.UnitID, st.row.SubjectType, st.row.Subject),
		payload: p, before: before, after: after, changed: changedPaths(before, after), baseState: st.state(),
	})
}

// applyMembershipTreeChange applies a held move or promotion inside the decision transaction, which
// already holds the graph and key locks.
func applyMembershipTreeChange(ctx context.Context, q store.Querier, ch types.GovernanceChange) (govApplied, error) {
	var p membershipTreePayload
	if err := json.Unmarshal(ch.Payload, &p); err != nil {
		return govApplied{}, fmt.Errorf("governance change %s: payload: %w", ch.ID, err)
	}
	st, err := readMembershipTreeState(ctx, q, p, true)
	if err != nil {
		return govApplied{}, err
	}
	if computeETag(st.state()) != ch.BaseHash {
		return govApplied{}, store.ErrGovernanceChangeStale
	}
	if ch.Op == "promote" {
		promoted, err := promoteMembershipQ(ctx, q, p.ID)
		if err != nil {
			return govApplied{}, err
		}
		return govApplied{action: "governance.membership.promote", target: promoted.ID.String(), data: membershipWriteAuditData(promoted)}, nil
	}
	from, moved, err := moveMembershipQ(ctx, q, p.ID, p.UnitID)
	if err != nil {
		return govApplied{}, err
	}
	return govApplied{action: "governance.membership.write", target: moved.ID.String(), data: membershipMoveAuditData(from, moved)}, nil
}

// unitActivation names a unit that may become someone's context.
type unitActivation struct {
	UnitID   uuid.UUID `json:"unit_id"`
	UnitName string    `json:"unit_name"`
}

// legacyActivations is the units whose legacy rows may take over for someone when legacy row d is
// deleted (DESIGN-TREE §5.2 H3): a person's first legacy row by precedence is their one active legacy
// row, so removing it lets the next one rank first. It is computed over the legacy rows of all,
// conservatively: a group row counts as matching anyone (no session's groups are known here), and two
// user rows may be one person's sub and email, whose order only that person's claims decide. Only two
// user_type rows of different types can never match one person.
func legacyActivations(d types.GovernanceMembership, all []types.GovernanceMembership, units []types.GovernanceUnit) []unitActivation {
	if !d.Legacy {
		return nil
	}
	byID := unitsByID(units)
	var out []unitActivation
	for _, l := range all {
		if !l.Legacy || l.ID == d.ID || l.UnitID == d.UnitID || !mayRankAfter(d, l, byID) {
			continue
		}
		if !slices.ContainsFunc(out, func(a unitActivation) bool { return a.UnitID == l.UnitID }) {
			out = append(out, unitActivation{UnitID: l.UnitID, UnitName: byID[l.UnitID].Name})
		}
	}
	slices.SortFunc(out, func(a, b unitActivation) int { return strings.Compare(a.UnitName, b.UnitName) })
	return out
}

// subjectTier is a membership's tier in the store's precedence order (store.subjectTierOrder).
func subjectTier(t types.CapabilitySubjectType) int {
	switch t {
	case types.CapabilitySubjectUser:
		return 0
	case types.CapabilitySubjectGroup:
		return 1
	case types.CapabilitySubjectUserType:
		return 2
	}
	return 3
}

// mayRankAfter reports whether l may match a person d matches and rank after d for them: the store's
// precedence (tier, then sub before email, then priority DESC, unit name ASC, subject ASC), read
// conservatively where it depends on a person's claims.
func mayRankAfter(d, l types.GovernanceMembership, byID map[uuid.UUID]types.GovernanceUnit) bool {
	if dt, lt := subjectTier(d.SubjectType), subjectTier(l.SubjectType); dt != lt {
		return lt > dt
	}
	if d.Subject != l.Subject {
		switch d.SubjectType {
		case types.CapabilitySubjectUser:
			return true // a sub and an email: the person's claims order them
		case types.CapabilitySubjectUserType:
			return false // a person has one type
		}
	}
	if d.Priority != l.Priority {
		return l.Priority < d.Priority
	}
	if dn, ln := byID[d.UnitID].Name, byID[l.UnitID].Name; dn != ln {
		return ln > dn
	}
	return l.Subject > d.Subject
}

// activationsAfterDelete is legacyActivations for a legacy row a direct delete removed, over the rows
// left. It is advisory: a failed read logs and lists none, since the delete has already committed.
func (s *Server) activationsAfterDelete(ctx context.Context, removed types.GovernanceMembership) []unitActivation {
	if !removed.Legacy {
		return nil
	}
	all, err := s.cfg.Store.ListGovernanceMemberships(ctx)
	if err == nil {
		var units []types.GovernanceUnit
		if units, err = s.cfg.Store.ListGovernanceUnits(ctx); err == nil {
			return legacyActivations(removed, all, units)
		}
	}
	slog.ErrorContext(ctx, "api: list the units a legacy membership delete may activate", "membership_id", removed.ID, "err", err)
	return nil
}
