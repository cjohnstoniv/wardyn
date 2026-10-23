// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// Restrictions ride in GovernanceDocument.Restrictions and are upserted by name, like a profile: a
// restriction in the document but absent server-side is created, one that differs is replaced (its
// members included), and with prune one the document omits is deleted. They are written over
// POST /governance/restrictions and PUT, DELETE /governance/restrictions/{id}.

// restrictionWrite is a document restriction as the write route takes it.
func restrictionWrite(r Restriction) types.RestrictionWrite {
	members := r.Members
	if members == nil {
		members = []types.RestrictionMember{}
	}
	return types.RestrictionWrite{Name: r.Name, Spec: r.Spec, Limits: r.Limits, Contact: r.Contact, Members: members}
}

// restrictionChangedFields is the JSON names of what writing r over existing changes, in a fixed order.
func restrictionChangedFields(existing, r Restriction) []string {
	var fields []string
	if !reflect.DeepEqual(existing.Spec, r.Spec) {
		fields = append(fields, "spec")
	}
	if !reflect.DeepEqual(existing.Limits, r.Limits) {
		fields = append(fields, "limits")
	}
	if !reflect.DeepEqual(existing.Contact, r.Contact) {
		fields = append(fields, "contact")
	}
	if !sameRestrictionMembers(existing.Members, r.Members) {
		fields = append(fields, "members")
	}
	return fields
}

func sameRestrictionMembers(a, b []types.RestrictionMember) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[types.RestrictionMember]bool, len(a))
	for _, m := range a {
		seen[m] = true
	}
	for _, m := range b {
		if !seen[m] {
			return false
		}
	}
	return true
}

// planRestrictions is the restriction steps of planGovernance.
func planRestrictions(current, doc GovernanceDocument, prune bool, plan *GovernancePlan) {
	byName := make(map[string]Restriction, len(current.Restrictions))
	for _, r := range current.Restrictions {
		byName[r.Name] = r
	}
	keep := make(map[string]bool, len(doc.Restrictions))
	for _, r := range doc.Restrictions {
		keep[r.Name] = true
		existing, ok := byName[r.Name]
		if !ok {
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "restriction", Op: "create", Name: r.Name})
			continue
		}
		fields := restrictionChangedFields(existing, r)
		if len(fields) == 0 {
			plan.Unchanged++
			continue
		}
		id := existing.ID
		plan.Steps = append(plan.Steps, ApplyStep{Kind: "restriction", Op: "update", Name: r.Name, ID: &id, Fields: fields})
	}
	if !prune {
		return
	}
	for _, r := range current.Restrictions {
		if !keep[r.Name] {
			id := r.ID
			plan.Steps = append(plan.Steps, ApplyStep{Kind: "restriction", Op: "delete", Name: r.Name, ID: &id})
		}
	}
}

// applyRestrictions upserts the document's restrictions and returns the saved rows in place. A write
// the server holds for approval is recorded in res.Pending.
func (c *Client) applyRestrictions(ctx context.Context, current GovernanceDocument, doc *GovernanceDocument, res *GovernanceApplyResult) error {
	byName := make(map[string]Restriction, len(current.Restrictions))
	for _, r := range current.Restrictions {
		byName[r.Name] = r
	}
	for i, r := range doc.Restrictions {
		existing, ok := byName[r.Name]
		if ok && len(restrictionChangedFields(existing, r)) == 0 {
			doc.Restrictions[i] = existing
			continue
		}
		method, path, step := http.MethodPost, "/api/v1/governance/restrictions", ApplyStep{Kind: "restriction", Op: "create", Name: r.Name}
		if ok {
			id := existing.ID
			method, path, step = http.MethodPut, path+"/"+id.String(), ApplyStep{Kind: "restriction", Op: "update", Name: r.Name, ID: &id}
		}
		var saved Restriction
		pending, err := c.doPending(ctx, method, path, restrictionWrite(r), &saved)
		if err != nil {
			err = fmt.Errorf("apply restriction %q: %w", r.Name, err)
		}
		res.Steps = append(res.Steps, step.withOutcome(pending != nil, err))
		if err != nil {
			return err
		}
		if pending != nil {
			res.Pending = append(res.Pending, *pending)
			continue
		}
		doc.Restrictions[i] = saved
	}
	return nil
}

// pruneRestrictions deletes every restriction in current that doc does not name, stopping at the first
// delete the server holds for approval (skipped=true).
func (c *Client) pruneRestrictions(ctx context.Context, current, doc GovernanceDocument, res *GovernanceApplyResult) (skipped bool, err error) {
	keep := make(map[string]bool, len(doc.Restrictions))
	for _, r := range doc.Restrictions {
		keep[r.Name] = true
	}
	for _, r := range current.Restrictions {
		if keep[r.Name] {
			continue
		}
		pending, err := c.doPending(ctx, http.MethodDelete, "/api/v1/governance/restrictions/"+r.ID.String(), nil, nil)
		if err != nil {
			err = fmt.Errorf("prune restriction %q: %w", r.Name, err)
		}
		id := r.ID
		res.Steps = append(res.Steps, ApplyStep{Kind: "restriction", Op: "delete", Name: r.Name, ID: &id}.withOutcome(pending != nil, err))
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

// SetWorkspaceOwningUnit pins workspace id to a unit, or unpins it with a nil unit. A move the server
// holds for a second approver returns a *PendingApprovalError. PUT /api/v1/workspaces/{id}/owning-unit.
func (c *Client) SetWorkspaceOwningUnit(ctx context.Context, id uuid.UUID, unit *uuid.UUID) (types.Workspace, error) {
	var out types.Workspace
	pending, err := c.doPending(ctx, http.MethodPut, "/api/v1/workspaces/"+id.String()+"/owning-unit",
		struct {
			UnitID *uuid.UUID `json:"unit_id"`
		}{unit}, &out)
	if err != nil {
		return types.Workspace{}, err
	}
	if pending != nil {
		return types.Workspace{}, &PendingApprovalError{Changes: []GovernanceChange{*pending}}
	}
	return out, nil
}
