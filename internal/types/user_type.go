// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import "time"

// UserTypeStandard is the id of the built-in user type the migration seeds:
// editable, never deletable, and the sign-in tie floor.
const UserTypeStandard = "standard"

// UserType is one row of the user_types table: an org-defined kind of person
// ("Portfolio manager"). It carries no controls of its own — governance
// assignments, capability grants and drive grants name it as a subject.
type UserType struct {
	// ID is the immutable slug role-map values, grant subjects and token
	// stamps refer to.
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Priority breaks a sign-in tie between two custom types (higher wins).
	// Always 0 on the built-in type, which never takes part in a tie.
	Priority  int       `json:"priority"`
	BuiltIn   bool      `json:"built_in"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}
