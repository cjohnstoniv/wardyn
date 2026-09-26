// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// CapabilitySubjectType names WHO a capability grant is written against
// (migration 0042). Closed and complete — its DB CHECK is pinned against these
// constants by internal/db's TestClosedEnumChecksMatchConstants.
type CapabilitySubjectType string

const (
	// CapabilitySubjectUser is one human, named by either their lowercased OIDC
	// "sub" or their email — a grant on EITHER matches, so an admin can write
	// down the identity they actually know rather than the one the IdP prefers.
	CapabilitySubjectUser CapabilitySubjectType = "user"
	// CapabilitySubjectGroup is one entry of the login-time union of the ID
	// token's roles+groups claims (see oidc.Session.Groups). Entra App Roles are
	// grantable through this without any extra configuration.
	CapabilitySubjectGroup CapabilitySubjectType = "group"
	// CapabilitySubjectAll is every signed-in human — the baseline for an IdP
	// that emits no usable groups claim. Subject is "" for this type.
	CapabilitySubjectAll CapabilitySubjectType = "all"
	// CapabilitySubjectUserType is everyone of one user type, named by the
	// type's id (UserType.ID). A person holds exactly one type, stamped at
	// sign-in. For the governance ceiling and drives it is a tier between
	// group and all; for capability grants it is one more subject, so a DENY
	// written against a type is a wall no user or group allow lifts.
	CapabilitySubjectUserType CapabilitySubjectType = "user_type"
)

// Valid reports whether t is one of the four subject types. Used to reject a
// garbage value at the API write boundary, mirroring ApprovalScope.Valid.
func (t CapabilitySubjectType) Valid() bool {
	switch t {
	case CapabilitySubjectUser, CapabilitySubjectGroup, CapabilitySubjectAll, CapabilitySubjectUserType:
		return true
	default:
		return false
	}
}

// CapabilityEffect is a grant's direction. Closed and complete; DENY BEATS
// ALLOW at resolution time, with no user-vs-group precedence — "Bob's user
// allow overrode the group deny" is a breach report, not a feature.
type CapabilityEffect string

const (
	CapabilityAllow CapabilityEffect = "allow"
	CapabilityDeny  CapabilityEffect = "deny"
)

// Valid reports whether e is allow or deny.
func (e CapabilityEffect) Valid() bool {
	return e == CapabilityAllow || e == CapabilityDeny
}

// CapabilityGrant is one row of the permissioning grant list (migration 0042):
// "subject S may (or may not) use capability C at value V".
//
// Capability is a PLAIN STRING here, not a typed enum, and that is deliberate:
// the closed kind set lives in exactly one Go slice in internal/api
// (capabilityKinds) and is validated at the write boundary, so a fifth kind is
// a constant plus a call site with no schema change. A stored kind this binary
// does not know is inert — no resolver ever asks for it.
//
// Value's meaning is per kind: a host or "*.suffix" for egress_host, an exact
// secret name / workspace uuid / image ref for the others, and "*" is the
// per-kind wildcard everywhere.
type CapabilityGrant struct {
	ID          uuid.UUID             `json:"id"`
	SubjectType CapabilitySubjectType `json:"subject_type"`
	Subject     string                `json:"subject"`
	Capability  string                `json:"capability"`
	Value       string                `json:"value"`
	Effect      CapabilityEffect      `json:"effect"`
	CreatedAt   time.Time             `json:"created_at"`
	CreatedBy   string                `json:"created_by,omitempty"`
}

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
type RoleMapping struct {
	ID        uuid.UUID `json:"id"`
	Value     string    `json:"value"`
	Role      string    `json:"role"`
	UserType  string    `json:"user_type,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	CreatedBy string    `json:"created_by,omitempty"`
}
