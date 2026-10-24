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
	mode, ok := s.unitWriteMode(w, r)
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
	var belongsBy []uuid.UUID
	if s.isUnitAdminCaller(r.Context()) {
		if belongsBy, ok = s.membershipAddInScope(w, r, a); !ok {
			return
		}
	}
	a.ID = uuid.New()
	a.CreatedBy = principalFromRequest(r)
	if mode == govQueue {
		// Every assignment change is held: a repoint can widen its subjects, and nothing here is
		// proven narrowing.
		s.holdMembershipUpsert(w, r, a, belongsBy)
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
	mode, ok := s.unitWriteMode(w, r)
	if !ok {
		return
	}
	id, ok := parseIDParam(w, r, "id", "governance assignment")
	if !ok {
		return
	}
	if s.isUnitAdminCaller(r.Context()) {
		s.deleteMembershipInScope(w, r, id)
		return
	}
	if mode == govQueue {
		// Deleting an assignment widens its subjects back to the deployment ceiling: never exempt.
		s.holdMembershipDelete(w, r, id, nil)
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
	membershipAlreadyMemberMsg   = "The subject is already a member of that unit; delete this membership instead"
	membershipAlreadyContext     = "This membership is already a context"
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
	mode, ok := s.unitWriteMode(w, r)
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
	if s.isUnitAdminCaller(r.Context()) {
		s.moveMembershipInScope(w, r, id, req.UnitID)
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
	var shift legacyShift
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (err error) {
		// The graph lock before the reads, so the state and the shift are read as the move would apply.
		if err = store.LockGovernanceGraph(ctx, q); err != nil {
			return err
		}
		if st, err = readMembershipTreeState(ctx, q, p, false); err != nil {
			return err
		}
		if shift, err = moveShiftQ(ctx, q, st.row, p.UnitID); err != nil {
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
		shift: shift,
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
// already holds the graph and key locks. A move whose legacy shift is no longer the one it was held
// with is stale (heldShiftStale).
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
	if st.row.Legacy {
		shift, err := moveShiftQ(ctx, q, st.row, p.UnitID)
		if err == nil {
			err = heldShiftStale(ch, shift)
		}
		if err != nil {
			return govApplied{}, err
		}
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

// legacyShift is what a legacy row's delete, move or priority re-post, or a unit's rename, may do to
// whose row wins (H3): the units it may make someone's context, and for any but a delete the units it
// may take someone out of.
type legacyShift struct {
	activates, vacates []unitActivation
}

// moveShiftQ is what moving legacy row row to unit to does to whose legacy row wins (legacyShiftQ),
// for each person row and another legacy row may both place whose order the move can change
// (nameTiesPair); nothing for a row that is not legacy. The caller holds the graph lock.
func moveShiftQ(ctx context.Context, q store.Querier, row types.GovernanceMembership, to uuid.UUID) (legacyShift, error) {
	if !row.Legacy {
		return legacyShift{}, nil
	}
	return legacyShiftQ(ctx, q, row, to, nameTiesPair, func() error {
		_, _, err := moveMembershipQ(ctx, q, row.ID, to)
		return err
	})
}

// upsertShiftQ is what writing a over legacy row cur, the row a's key already names, does to whose
// legacy row wins (legacyShiftQ): a new priority reorders cur against each legacy row it was tied or
// ordered with by priority or name (nameOrdersPair) whose priority lies from cur's old priority to its
// new one; any other keeps its order. Nothing for a new row (cur nil), which is never legacy, a cur
// that is not legacy, or one that keeps its priority: home is no key of the order.
func upsertShiftQ(ctx context.Context, q store.Querier, cur *types.GovernanceMembership, a types.GovernanceMembership) (legacyShift, error) {
	if cur == nil || !cur.Legacy || cur.Priority == a.Priority {
		return legacyShift{}, nil
	}
	lo, hi := min(cur.Priority, a.Priority), max(cur.Priority, a.Priority)
	pair := func(d, l types.GovernanceMembership) bool {
		return l.Priority >= lo && l.Priority <= hi && nameOrdersPair(d, l)
	}
	return legacyShiftQ(ctx, q, *cur, uuid.Nil, pair, func() error {
		_, err := store.UpsertGovernanceMembershipQ(ctx, q, a)
		return err
	})
}

// deleteActivationsQ is the units deleting legacy row row may make someone's context (legacyShiftQ),
// for each person row and another legacy row that may rank after it may both place (mayFollowPair);
// none for a row that is not legacy. The caller holds the graph lock.
func deleteActivationsQ(ctx context.Context, q store.Querier, row types.GovernanceMembership) ([]unitActivation, error) {
	if !row.Legacy {
		return nil, nil
	}
	shift, err := legacyShiftQ(ctx, q, row, uuid.Nil, mayFollowPair, func() error { return store.DeleteGovernanceMembershipQ(ctx, q, row.ID) })
	return shift.activates, err
}

// legacyShiftQ is what change, a move of legacy row d to unit to or its delete (to nil), does to whose
// legacy row wins (H3), in the resolver's own order (winnersAroundQ). For each person d and another
// legacy row l may both place (pair, nil for every l), it reads the winner before the change and after
// it. A winner in another unit activates that unit, and a winner d overtakes vacates its unit. A
// person who goes from d's unit to unit to is the move's own change, which pathNarrowsQ proves.
func legacyShiftQ(ctx context.Context, q store.Querier, d types.GovernanceMembership, to uuid.UUID,
	pair func(d, l types.GovernanceMembership) bool, change func() error) (legacyShift, error) {
	all, err := store.ListGovernanceMembershipsQ(ctx, q)
	if err != nil {
		return legacyShift{}, err
	}
	was, now, err := winnersAroundQ(ctx, q, legacyPeople([]types.GovernanceMembership{d}, all, pair), change)
	if err != nil {
		return legacyShift{}, err
	}
	own := func(b, a store.MembershipMatch) bool { return b.Unit.ID == d.UnitID && a.Unit.ID == to }
	return shiftBetween(was, now, own, func(a store.MembershipMatch) bool { return a.Membership.ID == d.ID }), nil
}

// renameShiftQ is what renaming unit id to name does to whose legacy row wins (H3), in the resolver's
// own order (winnersAroundQ): the name breaks a tie between legacy rows of one tier and priority. A
// person's winner can move only between their first legacy row in the unit and their first elsewhere
// when the two tie up to the name (nameTiesPair), and which way depends on the other row's unit alone.
// So it reads, before the rename and after it, one person for each legacy row of another unit tied
// with a row of the unit (store.LegacyRenameRivalsQ), not one for each tied pair; none when no such
// pair exists, renameMaySelect's test. A winner in another unit activates that unit, and the unit's
// winning over another unit vacates that one. The caller holds the graph lock.
func renameShiftQ(ctx context.Context, q store.Querier, id uuid.UUID, name string) (legacyShift, error) {
	rivals, err := store.LegacyRenameRivalsQ(ctx, q, id)
	if err != nil {
		return legacyShift{}, err
	}
	was, now, err := winnersAroundQ(ctx, q, peoplePlacing(rivals), func() error {
		return store.RenameGovernanceUnitQ(ctx, q, id, name)
	})
	if err != nil {
		return legacyShift{}, err
	}
	return shiftBetween(was, now, nil, func(a store.MembershipMatch) bool { return a.Unit.ID == id }), nil
}

// unitRenameShiftQ is renameShiftQ for unit id written with name over its row in all, the graph-locked
// transaction q's units; nothing for a new unit or one that keeps its name.
func unitRenameShiftQ(ctx context.Context, q store.Querier, all []types.GovernanceUnit, id uuid.UUID, name string) (legacyShift, error) {
	if cur, ok := unitsByID(all)[id]; !ok || cur.Name == name {
		return legacyShift{}, nil
	}
	return renameShiftQ(ctx, q, id, name)
}

// winnersAroundQ is each of people's winning legacy row before change and after it (legacyWinnersQ),
// the resolver's own order, never a copy of it: names sort under the database's collation. change is
// applied in a savepoint and always rolled back; it is not run when there is no one to read.
func winnersAroundQ(ctx context.Context, q store.Querier, people []legacyClaims, change func() error) (was, now []store.MembershipMatch, err error) {
	if len(people) == 0 {
		return nil, nil, nil
	}
	if was, err = legacyWinnersQ(ctx, q, people); err != nil {
		return nil, nil, err
	}
	if _, err := q.Exec(ctx, `SAVEPOINT legacy_shift`); err != nil {
		return nil, nil, fmt.Errorf("api: savepoint for a legacy shift: %w", err)
	}
	if err = change(); err == nil {
		now, err = legacyWinnersQ(ctx, q, people)
	}
	if _, rerr := q.Exec(ctx, `ROLLBACK TO SAVEPOINT legacy_shift`); err == nil && rerr != nil {
		err = fmt.Errorf("api: roll back a legacy shift: %w", rerr)
	}
	if err != nil {
		return nil, nil, err
	}
	return was, now, nil
}

// shiftBetween is a shift's answer from each person's winner before the change (was) and after it
// (now): the new winner's unit activates, and when the changed row overtook (overtakes), the old
// winner's unit vacates. A person whose winners own reports (nil for none) is the change's own.
func shiftBetween(was, now []store.MembershipMatch, own func(b, a store.MembershipMatch) bool, overtakes func(a store.MembershipMatch) bool) legacyShift {
	var shift legacyShift
	add := func(list []unitActivation, u types.GovernanceUnit) []unitActivation {
		if slices.ContainsFunc(list, func(a unitActivation) bool { return a.UnitID == u.ID }) {
			return list
		}
		return append(list, unitActivation{UnitID: u.ID, UnitName: u.Name})
	}
	for i := range was {
		b, a := was[i], now[i]
		if a.Membership.ID == uuid.Nil || b.Unit.ID == a.Unit.ID || (own != nil && own(b, a)) {
			continue
		}
		shift.activates = add(shift.activates, a.Unit)
		if overtakes(a) {
			shift.vacates = add(shift.vacates, b.Unit)
		}
	}
	for _, list := range [][]unitActivation{shift.activates, shift.vacates} {
		slices.SortFunc(list, func(a, b unitActivation) int { return strings.Compare(a.UnitName, b.UnitName) })
	}
	return shift
}

// legacyClaims is a person's claims as store.ListMembershipsForQ matches them.
type legacyClaims struct {
	users, groups []string
	userType      string
}

// claimsPlacing is the claims of the person legacy rows d and l both place and no other subject does,
// d's user subject first: of a sub and an email only the person's claims order the two, and with d's
// first, d ranks before l. ok is false when no one person matches both (two user types).
func claimsPlacing(d, l types.GovernanceMembership) (c legacyClaims, ok bool) {
	for _, m := range []types.GovernanceMembership{d, l} {
		switch m.SubjectType {
		case types.CapabilitySubjectUser:
			if !slices.Contains(c.users, m.Subject) {
				c.users = append(c.users, m.Subject)
			}
		case types.CapabilitySubjectGroup:
			if !slices.Contains(c.groups, m.Subject) {
				c.groups = append(c.groups, m.Subject)
			}
		case types.CapabilitySubjectUserType:
			if c.userType != "" && c.userType != m.Subject {
				return c, false
			}
			c.userType = m.Subject
		}
	}
	slices.Sort(c.groups)
	return c, true
}

// legacyPeople is the claims of each person a legacy row of ds and another legacy row of all both place
// (pair, nil for every row), once each (peoplePlacing).
func legacyPeople(ds, all []types.GovernanceMembership, pair func(d, l types.GovernanceMembership) bool) []legacyClaims {
	var pairs [][2]types.GovernanceMembership
	for _, d := range ds {
		for _, l := range all {
			if l.Legacy && l.ID != d.ID && (pair == nil || pair(d, l)) {
				pairs = append(pairs, [2]types.GovernanceMembership{d, l})
			}
		}
	}
	return peoplePlacing(pairs)
}

// peoplePlacing is the claims of the person each pair's two rows both place (claimsPlacing), once each.
func peoplePlacing(pairs [][2]types.GovernanceMembership) []legacyClaims {
	var out []legacyClaims
	seen := map[string]bool{}
	for _, p := range pairs {
		c, ok := claimsPlacing(p[0], p[1])
		if key := fmt.Sprintf("%q %q %q", c.users, c.groups, c.userType); ok && !seen[key] {
			seen[key] = true
			out = append(out, c)
		}
	}
	return out
}

// legacyWinnersQ is each person's winning legacy row on q as the resolver picks it: the legacy row
// activeMemberships keeps of store.ListMembershipsForQ's rows, its first in that ORDER BY
// (store.LegacyWinnersQ, one statement for every person). The zero match for a person no legacy row
// places.
func legacyWinnersQ(ctx context.Context, q store.Querier, people []legacyClaims) ([]store.MembershipMatch, error) {
	claims := make([]store.MembershipClaims, len(people))
	for i, c := range people {
		claims[i] = store.MembershipClaims{Users: c.users, Groups: c.groups, UserType: c.userType}
	}
	return store.LegacyWinnersQ(ctx, q, claims)
}

// nameOrdersPair reports whether one person may match legacy rows a and b with their order decided at
// or after the priority, which a priority re-post changes, and so at the unit name, the one key a move
// or a rename changes: rows of one tier, and two group rows or two rows naming one subject. A sub and
// an email are ordered by the person's claims and two user types match no one person.
func nameOrdersPair(a, b types.GovernanceMembership) bool {
	return a.SubjectType == b.SubjectType && (a.SubjectType == types.CapabilitySubjectGroup || a.Subject == b.Subject)
}

// nameTiesPair is nameOrdersPair for rows of one priority, the pairs whose order is decided at the
// unit name. A move or a rename changes no other key, so only these can trade places: a person whose
// winner changes has the old and the new winner tied up to the name, and the person those two alone
// place reads the same two winners, as their rows are a subset of this person's.
func nameTiesPair(a, b types.GovernanceMembership) bool {
	return a.Priority == b.Priority && nameOrdersPair(a, b)
}

// mayFollowPair reports whether legacy row b may rank after legacy row a for some person: all but a
// row nameOrdersPair orders with a at a higher priority, which ranks before a for anyone. Deleting a
// hands a person their next row only where a won, and the person a and that row alone place reads the
// same two winners.
func mayFollowPair(a, b types.GovernanceMembership) bool {
	return !nameOrdersPair(a, b) || b.Priority <= a.Priority
}

// heldShiftStale answers stale when now, what held change ch's delete or move of a legacy row or its
// unit rename would do as read on the decision's transaction under its graph lock, is not what ch was
// held with (H3). Its approver was authorised over the stored units (unitChangeScope), and its base
// covers only the row and its unit chains, so an edit of another legacy row since must not let it
// apply over others.
func heldShiftStale(ch types.GovernanceChange, now legacyShift) error {
	var held governanceDiffBody
	if err := json.Unmarshal(ch.Diff, &held); err != nil {
		return fmt.Errorf("governance change %s: diff: %w", ch.ID, err)
	}
	if !sameUnits(now.activates, held.Activates) || !sameUnits(now.vacates, held.Vacates) {
		return store.ErrGovernanceChangeStale
	}
	return nil
}

// sameUnits reports whether a and b, each listing a unit once, list the same units.
func sameUnits(a, b []unitActivation) bool {
	if len(a) != len(b) {
		return false
	}
	for _, x := range a {
		if !slices.ContainsFunc(b, func(y unitActivation) bool { return y.UnitID == x.UnitID }) {
			return false
		}
	}
	return true
}

// activationsAfterDelete is deleteActivationsQ for a legacy row a direct delete removed, read after
// the commit on a rolled-back transaction that puts the row back first, so the order is the
// resolver's as it stood before the delete. It is advisory: a failed read logs and lists none, since
// the delete has already committed.
func (s *Server) activationsAfterDelete(ctx context.Context, removed types.GovernanceMembership) []unitActivation {
	if !removed.Legacy {
		return nil
	}
	var activates []unitActivation
	err := s.cfg.Store.DryRunGovernance(ctx, func(q store.Querier) (err error) {
		if err = store.InsertGovernanceMembershipQ(ctx, q, removed); err != nil {
			return err
		}
		activates, err = deleteActivationsQ(ctx, q, removed)
		return err
	})
	if err != nil {
		slog.ErrorContext(ctx, "api: list the units a legacy membership delete may activate", "membership_id", removed.ID, "err", err)
		return nil
	}
	return activates
}
