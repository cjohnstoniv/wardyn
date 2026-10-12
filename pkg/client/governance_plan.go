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
	profileByName := make(map[string]GovernanceProfile, len(current.Profiles))
	currentIDToName := make(map[uuid.UUID]string, len(current.Profiles))
	for _, p := range current.Profiles {
		profileByName[p.Name] = p
		currentIDToName[p.ID] = p.Name
	}
	assignmentByKey := make(map[string]GovernanceAssignment, len(current.Assignments))
	for _, a := range current.Assignments {
		assignmentByKey[governanceAssignmentKey(a.SubjectType, a.Subject)] = a
	}
	fileIDToName := make(map[uuid.UUID]string, len(doc.Profiles))
	for _, p := range doc.Profiles {
		if p.ID != uuid.Nil {
			fileIDToName[p.ID] = p.Name
		}
	}
	order, err := governanceWriteOrder(doc.Profiles, fileIDToName)
	if err != nil {
		return GovernancePlan{}, err
	}

	for _, i := range order {
		p := doc.Profiles[i]
		existing, ok := profileByName[p.Name]
		if !ok {
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "profile", Op: "create", Name: p.Name})
			continue
		}
		q := p
		if q.BaseProfileID != nil {
			if baseName, ok := fileIDToName[*q.BaseProfileID]; ok {
				// A base not on the server by name is created with a fresh id, which
				// apply points this profile at; no id the server holds can equal it.
				id := uuid.Nil
				if base, ok := profileByName[baseName]; ok {
					id = base.ID
				}
				q.BaseProfileID = &id
			}
		}
		fields := governanceProfileChangedFields(existing, q)
		if len(fields) == 0 {
			plan.Unchanged++
			continue
		}
		id := existing.ID
		plan.Steps = append(plan.Steps, ApplyStep{Kind: "profile", Op: "update", Name: p.Name, ID: &id, Fields: fields})
	}

	for _, a := range doc.Assignments {
		name := fileIDToName[a.ProfileID]
		if name == "" {
			name = currentIDToName[a.ProfileID]
		}
		profile := name
		if profile == "" {
			profile = a.ProfileID.String()
		}
		existing, ok := assignmentByKey[governanceAssignmentKey(a.SubjectType, a.Subject)]
		if !ok {
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "assignment", Op: "create",
				SubjectType: a.SubjectType, Subject: a.Subject, Profile: profile})
			continue
		}
		sameProfile := (name != "" && currentIDToName[existing.ProfileID] == name) ||
			(name == "" && existing.ProfileID == a.ProfileID)
		var fields []string
		if !sameProfile {
			fields = append(fields, "profile")
		}
		if existing.Priority != a.Priority {
			fields = append(fields, "priority")
		}
		if len(fields) == 0 {
			plan.Unchanged++
			continue
		}
		id := existing.ID
		plan.Steps = append(plan.Steps, ApplyStep{Kind: "assignment", Op: "update",
			SubjectType: a.SubjectType, Subject: a.Subject, Profile: profile, ID: &id, Fields: fields})
	}

	if prune {
		keepAssignment := make(map[string]bool, len(doc.Assignments))
		for _, a := range doc.Assignments {
			keepAssignment[governanceAssignmentKey(a.SubjectType, a.Subject)] = true
		}
		for _, a := range current.Assignments {
			if keepAssignment[governanceAssignmentKey(a.SubjectType, a.Subject)] {
				continue
			}
			id := a.ID
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "assignment", Op: "delete",
				SubjectType: a.SubjectType, Subject: a.Subject, Profile: currentIDToName[a.ProfileID], ID: &id})
		}
		keepProfile := make(map[string]bool, len(doc.Profiles))
		for _, p := range doc.Profiles {
			keepProfile[p.Name] = true
		}
		for _, p := range governancePruneOrder(current.Profiles) {
			if keepProfile[p.Name] {
				continue
			}
			id := p.ID
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "profile", Op: "delete", Name: p.Name, ID: &id})
		}
	}
	plan.Destructive = slices.ContainsFunc(plan.Steps, func(s ApplyStep) bool { return s.Op == "delete" })
	return plan, nil
}
