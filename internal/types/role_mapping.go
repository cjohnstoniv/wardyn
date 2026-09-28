// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// RoleMapping is one console-managed (Getting Started -> People) row of
// migration 0051's role_mappings table: "value maps to role". This is the
// STORE'S wire type, carrying id/timestamps/provenance — distinct on purpose
// from internal/auth/oidc's own RoleMapping (just Value/Role), which stays
// dependency-free of this package (see that type's doc comment) the same way
// oidc.SessionRevocations keeps oidc dependency-free of store; the API layer
// converts between the two, mirroring however SessionRevocations bridges
// store -> oidc today.
//
// Value is expected already canonical (trimmed, lowercased, ASCII) by the API
// write boundary that owns writes to this table — see the migration comment.
//
// UserType is the row's user type when Role is the user tier (migration
// 0070_user_tier_rename's column, a foreign key to user_types); "" on a tier
// row, and on a user row written before types existed, which reads as the
// built-in "standard".
//
// MigratedFromMember (migration 0098) marks a row that rewrite rewrote from
// role='member' rather than one an admin actually saved on the built-in type
// — the store's UpsertRoleMapping clears it on any write that flips the row
// in place, since choosing a real type is what the marker exists to prompt.
type RoleMapping struct {
	ID                 uuid.UUID `json:"id"`
	Value              string    `json:"value"`
	Role               string    `json:"role"`
	UserType           string    `json:"user_type,omitempty"`
	MigratedFromMember bool      `json:"migrated_from_member,omitempty"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedBy          string    `json:"created_by,omitempty"`
}
