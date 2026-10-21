// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"slices"

	"github.com/google/uuid"
)

// PlanGovernance returns the writes ApplyGovernance(ctx, doc, prune) would make
// against the current state, without making them. It is read-only: one GET
// /api/v1/governance, and no write request. Its matching rules are
// ApplyGovernance's, so the create and update steps are the writes an apply
// issues against the same state. A plan is a snapshot: the server can change
// before an apply runs.
func (c *Client) PlanGovernance(ctx context.Context, doc GovernanceDocument, prune bool) (GovernancePlan, error) {
	current, err := c.GetGovernance(ctx)
	if err != nil {
		return GovernancePlan{}, fmt.Errorf("read current governance state: %w", err)
	}
	return planGovernance(current, doc, prune)
}

// planGovernance is PlanGovernance's pure core: the steps that turn current
// into doc, in the order an apply would issue them.
func planGovernance(current, doc GovernanceDocument, prune bool) (GovernancePlan, error) {
	plan := GovernancePlan{Steps: make([]ApplyStep, 0)}
	profileByName := make(map[string]GovernanceUnit, len(current.Units))
	currentIDToName := make(map[uuid.UUID]string, len(current.Units))
	for _, p := range current.Units {
		profileByName[p.Name] = p
		currentIDToName[p.ID] = p.Name
	}
	assignmentByKey := make(map[string]GovernanceMembership, len(current.Memberships))
	for _, a := range current.Memberships {
		assignmentByKey[governanceMembershipKey(a.SubjectType, a.Subject)] = a
	}
	fileIDToName := make(map[uuid.UUID]string, len(doc.Units))
	for _, p := range doc.Units {
		if p.ID != uuid.Nil {
			fileIDToName[p.ID] = p.Name
		}
	}
	order, err := governanceWriteOrder(doc.Units, fileIDToName)
	if err != nil {
		return GovernancePlan{}, err
	}

	for _, i := range order {
		p := doc.Units[i]
		existing, ok := profileByName[p.Name]
		if !ok {
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "unit", Op: "create", Name: p.Name})
			continue
		}
		q := p
		if q.ParentID != nil {
			if baseName, ok := fileIDToName[*q.ParentID]; ok {
				// A base not on the server by name is created with a fresh id, which
				// apply points this profile at; no id the server holds can equal it.
				id := uuid.Nil
				if base, ok := profileByName[baseName]; ok {
					id = base.ID
				}
				q.ParentID = &id
			}
		}
		fields := governanceUnitChangedFields(existing, q)
		if len(fields) == 0 {
			plan.Unchanged++
			continue
		}
		id := existing.ID
		plan.Steps = append(plan.Steps, ApplyStep{Kind: "unit", Op: "update", Name: p.Name, ID: &id, Fields: fields})
	}

	for _, a := range doc.Memberships {
		name := fileIDToName[a.UnitID]
		if name == "" {
			name = currentIDToName[a.UnitID]
		}
		profile := name
		if profile == "" {
			profile = a.UnitID.String()
		}
		existing, ok := assignmentByKey[governanceMembershipKey(a.SubjectType, a.Subject)]
		if !ok {
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "membership", Op: "create",
				SubjectType: a.SubjectType, Subject: a.Subject, Unit: profile})
			continue
		}
		sameUnit := (name != "" && currentIDToName[existing.UnitID] == name) ||
			(name == "" && existing.UnitID == a.UnitID)
		var fields []string
		if !sameUnit {
			fields = append(fields, "unit")
		}
		if existing.Priority != a.Priority {
			fields = append(fields, "priority")
		}
		if len(fields) == 0 {
			plan.Unchanged++
			continue
		}
		id := existing.ID
		plan.Steps = append(plan.Steps, ApplyStep{Kind: "membership", Op: "update",
			SubjectType: a.SubjectType, Subject: a.Subject, Unit: profile, ID: &id, Fields: fields})
	}

	if prune {
		keepAssignment := make(map[string]bool, len(doc.Memberships))
		for _, a := range doc.Memberships {
			keepAssignment[governanceMembershipKey(a.SubjectType, a.Subject)] = true
		}
		for _, a := range current.Memberships {
			if keepAssignment[governanceMembershipKey(a.SubjectType, a.Subject)] {
				continue
			}
			id := a.ID
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "membership", Op: "delete",
				SubjectType: a.SubjectType, Subject: a.Subject, Unit: currentIDToName[a.UnitID], ID: &id})
		}
		keepProfile := make(map[string]bool, len(doc.Units))
		for _, p := range doc.Units {
			keepProfile[p.Name] = true
		}
		for _, p := range governancePruneOrder(current.Units) {
			if keepProfile[p.Name] {
				continue
			}
			id := p.ID
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "unit", Op: "delete", Name: p.Name, ID: &id})
		}
	}
	plan.Destructive = slices.ContainsFunc(plan.Steps, func(s ApplyStep) bool { return s.Op == "delete" })
	return plan, nil
}
