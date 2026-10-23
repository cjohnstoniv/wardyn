// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// GovernanceUnitAdmin is one unit-admin grant: a person (subject_type user) or an IdP group that
// governs a unit and every unit below it. GET /api/v1/governance lists them as unit_admins.
type GovernanceUnitAdmin = types.GovernanceUnitAdmin

// GovernanceAdminSubject is one admin of a unit as the governance file names it.
type GovernanceAdminSubject struct {
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
}

// GovernanceChangeApprover is one who could decide a held change: kind "security" (any security
// admin) or "unit_admin" (the grant Name names, on UnitName; SubjectType says person or group).
type GovernanceChangeApprover struct {
	Kind        string                `json:"kind"`
	Name        string                `json:"name,omitempty"`
	UnitName    string                `json:"unit_name,omitempty"`
	SubjectType CapabilitySubjectType `json:"subject_type,omitempty"`
}

// GrantUnitAdmin makes subject an admin of the unit unitID and the units below it. POST
// /api/v1/governance/units/{id}/admins. A grant the server holds for approval (always, when the
// caller is a unit admin) comes back as the pending change and a zero grant.
func (c *Client) GrantUnitAdmin(ctx context.Context, unitID uuid.UUID, subjectType CapabilitySubjectType, subject string) (GovernanceUnitAdmin, *GovernanceChange, error) {
	var out GovernanceUnitAdmin
	body := GovernanceAdminSubject{SubjectType: subjectType, Subject: subject}
	pending, err := c.doPending(ctx, http.MethodPost, "/api/v1/governance/units/"+unitID.String()+"/admins", body, &out)
	return out, pending, err
}

// RevokeUnitAdmin removes the grant adminID from the unit unitID. DELETE
// /api/v1/governance/units/{id}/admins/{admin}. A revoke the server holds for approval comes back as
// the pending change.
func (c *Client) RevokeUnitAdmin(ctx context.Context, unitID, adminID uuid.UUID) (*GovernanceChange, error) {
	return c.doPending(ctx, http.MethodDelete, "/api/v1/governance/units/"+unitID.String()+"/admins/"+adminID.String(), nil, nil)
}

// governanceRead is GET /api/v1/governance as the wire carries it: the document plus the grant list,
// which GetGovernance folds into the document's admins by unit name.
type governanceRead struct {
	GovernanceDocument
	UnitAdmins []GovernanceUnitAdmin `json:"unit_admins"`
}

// adminsByUnitName is grants as a document's admins: by unit name (the unit id when no unit of units
// has it), each list in subject order. Nil for no grant.
func adminsByUnitName(units []GovernanceUnit, grants []GovernanceUnitAdmin) map[string][]GovernanceAdminSubject {
	if len(grants) == 0 {
		return nil
	}
	names := make(map[uuid.UUID]string, len(units))
	for _, u := range units {
		names[u.ID] = u.Name
	}
	out := map[string][]GovernanceAdminSubject{}
	for _, g := range grants {
		name := cmpOr(names[g.UnitID], g.UnitID.String())
		out[name] = append(out[name], GovernanceAdminSubject{SubjectType: g.SubjectType, Subject: g.Subject})
	}
	for _, subjects := range out {
		slices.SortFunc(subjects, func(a, b GovernanceAdminSubject) int {
			return strings.Compare(string(a.SubjectType)+"\x00"+a.Subject, string(b.SubjectType)+"\x00"+b.Subject)
		})
	}
	return out
}

// unitAdminKey is the key a document admin and a stored grant are matched by: the unit's name and
// the subject folded as the server stores it (governanceMembershipKey's rule).
func unitAdminKey(unit string, subjectType CapabilitySubjectType, subject string) string {
	return governanceMembershipKey(unit, subjectType, subject)
}

// adminPlan is what an apply needs to match the document's admins against the server's grants.
type adminPlan struct {
	byKey           map[string]GovernanceUnitAdmin // the server's grants, by unitAdminKey
	currentIDToName map[uuid.UUID]string
	unitID          func(name string) (uuid.UUID, bool) // the unit's id on the server after the unit writes
	pendingProfile  map[string]bool
}

func newAdminPlan(current GovernanceDocument, unitID func(string) (uuid.UUID, bool), pending map[string]bool) adminPlan {
	p := adminPlan{byKey: map[string]GovernanceUnitAdmin{}, currentIDToName: map[uuid.UUID]string{}, unitID: unitID, pendingProfile: pending}
	for _, u := range current.Units {
		p.currentIDToName[u.ID] = u.Name
	}
	for _, g := range current.grants {
		p.byKey[unitAdminKey(membershipUnitName(g.UnitID, nil, p.currentIDToName), g.SubjectType, g.Subject)] = g
	}
	return p
}

// sortedUnitNames is the document's admin units in name order, so an apply and a plan issue the
// grants in the same order every time.
func sortedUnitNames(admins map[string][]GovernanceAdminSubject) []string {
	names := make([]string, 0, len(admins))
	for name := range admins {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// applyUnitAdmins grants every admin the document names that the server lacks. A unit whose own
// write is pending defers its grants; a grant the server holds is recorded in res.Pending.
func (c *Client) applyUnitAdmins(ctx context.Context, plan adminPlan, admins map[string][]GovernanceAdminSubject, res *GovernanceApplyResult) error {
	for _, name := range sortedUnitNames(admins) {
		for _, a := range admins[name] {
			if _, ok := plan.byKey[unitAdminKey(name, a.SubjectType, a.Subject)]; ok {
				continue
			}
			if plan.pendingProfile[name] {
				res.Deferred = append(res.Deferred, GovernanceDeferredWrite{SubjectType: a.SubjectType, Subject: a.Subject, Unit: name})
				continue
			}
			step := ApplyStep{Kind: "unit_admin", Op: "create", SubjectType: a.SubjectType, Subject: a.Subject, Unit: name}
			id, ok := plan.unitID(name)
			if !ok {
				err := fmt.Errorf("apply unit admin (%s %q): no unit named %q in the document or on the server", a.SubjectType, a.Subject, name)
				res.Steps = append(res.Steps, step.withOutcome(false, err))
				return err
			}
			_, pending, err := c.GrantUnitAdmin(ctx, id, a.SubjectType, a.Subject)
			if err != nil {
				err = fmt.Errorf("apply unit admin (%s %q) on %q: %w", a.SubjectType, a.Subject, name, err)
			}
			res.Steps = append(res.Steps, step.withOutcome(pending != nil, err))
			if err != nil {
				return err
			}
			if pending != nil {
				res.Pending = append(res.Pending, *pending)
			}
		}
	}
	return nil
}

// pruneUnitAdmins revokes every grant on the server the document's admins do not name. It stops at
// the first revoke the server holds (recorded in res.Pending) and reports skipped.
func (c *Client) pruneUnitAdmins(ctx context.Context, plan adminPlan, admins map[string][]GovernanceAdminSubject, res *GovernanceApplyResult) (skipped bool, err error) {
	for _, g := range unitAdminsToPrune(plan, admins) {
		name := membershipUnitName(g.UnitID, nil, plan.currentIDToName)
		pending, err := c.RevokeUnitAdmin(ctx, g.UnitID, g.ID)
		if err != nil {
			err = fmt.Errorf("prune unit admin (%s %q) on %q: %w", g.SubjectType, g.Subject, name, err)
		}
		id := g.ID
		step := ApplyStep{Kind: "unit_admin", Op: "delete", SubjectType: g.SubjectType, Subject: g.Subject, Unit: name, ID: &id}
		res.Steps = append(res.Steps, step.withOutcome(pending != nil, err))
		if err != nil {
			return false, err
		}
		if pending != nil {
			res.Pending = append(res.Pending, *pending)
			return true, nil
		}
	}
	return false, nil
}

// unitAdminsToPrune is the server's grants the document's admins do not name, in unit then subject
// order.
func unitAdminsToPrune(plan adminPlan, admins map[string][]GovernanceAdminSubject) []GovernanceUnitAdmin {
	keep := map[string]bool{}
	for name, subjects := range admins {
		for _, a := range subjects {
			keep[unitAdminKey(name, a.SubjectType, a.Subject)] = true
		}
	}
	var out []GovernanceUnitAdmin
	for key, g := range plan.byKey {
		if !keep[key] {
			out = append(out, g)
		}
	}
	slices.SortFunc(out, func(a, b GovernanceUnitAdmin) int {
		return strings.Compare(unitAdminKey(plan.currentIDToName[a.UnitID], a.SubjectType, a.Subject),
			unitAdminKey(plan.currentIDToName[b.UnitID], b.SubjectType, b.Subject))
	})
	return out
}

// planUnitAdmins is the grant and revoke steps an apply of admins would make against plan.
func planUnitAdmins(plan adminPlan, admins map[string][]GovernanceAdminSubject, prune bool) (steps []ApplyStep, unchanged int) {
	for _, name := range sortedUnitNames(admins) {
		for _, a := range admins[name] {
			if _, ok := plan.byKey[unitAdminKey(name, a.SubjectType, a.Subject)]; ok {
				unchanged++
				continue
			}
			steps = append(steps, ApplyStep{Kind: "unit_admin", Op: "create", SubjectType: a.SubjectType, Subject: a.Subject, Unit: name})
		}
	}
	if prune {
		for _, g := range unitAdminsToPrune(plan, admins) {
			id := g.ID
			steps = append(steps, ApplyStep{Kind: "unit_admin", Op: "delete", SubjectType: g.SubjectType, Subject: g.Subject,
				Unit: membershipUnitName(g.UnitID, nil, plan.currentIDToName), ID: &id})
		}
	}
	return steps, unchanged
}

// UnitAdminGrants is the unit-admin grants, with their ids, as the GetGovernance that returned d read
// them; nil for a document not read from the server.
func (d GovernanceDocument) UnitAdminGrants() []GovernanceUnitAdmin { return slices.Clone(d.grants) }
