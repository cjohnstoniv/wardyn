// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

import "github.com/google/uuid"

// ApplyStep is one write a declarative apply would make (a plan) or made (an apply result).
type ApplyStep struct {
	Kind        string                `json:"kind"`                   // "profile" | "assignment" | "drive" | "drive_grant"
	Op          string                `json:"op"`                     // "create" | "update" | "delete" | "upsert"
	Name        string                `json:"name,omitempty"`         // profile or drive name
	SubjectType CapabilitySubjectType `json:"subject_type,omitempty"` // assignment / drive_grant
	Subject     string                `json:"subject,omitempty"`
	Profile     string                `json:"profile,omitempty"` // an assignment's profile name; its id when no name is known
	ID          *uuid.UUID            `json:"id,omitempty"`      // the existing row an update or delete targets
	Fields      []string              `json:"fields,omitempty"`  // update: the JSON names of what changes
	Outcome     string                `json:"outcome,omitempty"` // results only: "applied" | "pending" | "failed"
	Error       string                `json:"error,omitempty"`   // with outcome "failed"
}

// withOutcome records how the write this step describes went: failed when it
// errored, pending when the server held it for approval, applied otherwise.
func (s ApplyStep) withOutcome(pending bool, err error) ApplyStep {
	switch {
	case err != nil:
		s.Outcome, s.Error = "failed", err.Error()
	case pending:
		s.Outcome = "pending"
	default:
		s.Outcome = "applied"
	}
	return s
}

// GovernancePlan is PlanGovernance's answer. Steps is never null.
type GovernancePlan struct {
	Steps       []ApplyStep `json:"steps"`
	Unchanged   int         `json:"unchanged"`
	Destructive bool        `json:"destructive"` // true when any step is a delete
}
