// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Governance memberships: the writes binding a subject to a unit.
package api

import (
	"fmt"
	"math"
	"net/http"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/auth/oidc"
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
	s.recordAudit(r.Context(), s.auditEvent(nil, actorTypeFromRequest(r), principalFromRequest(r),
		"governance.membership.delete", id.String(), "success", mustJSON(membershipWriteAuditData(removed))))
	if mode == govBypass {
		s.recordGovernanceBypass(r, govKindMembership, id.String(), "success", nil)
	}
	w.WriteHeader(http.StatusNoContent)
}
