// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// CapabilitySubjectType names WHO a capability grant is written against.
// Closed and complete — its DB CHECK is pinned against these constants by
// internal/db's TestClosedEnumChecksMatchConstants.
type CapabilitySubjectType string

const (
	// CapabilitySubjectUser is one human, named by lowercased OIDC "sub" or
	// email — either matches, so an admin can record whichever identity they
	// actually know.
	CapabilitySubjectUser CapabilitySubjectType = "user"
	// CapabilitySubjectGroup is one entry of the login-time union of the ID
	// token's roles+groups claims (oidc.Session.Groups); Entra App Roles are
	// grantable through this with no extra config.
	CapabilitySubjectGroup CapabilitySubjectType = "group"
	// CapabilitySubjectAll is every signed-in human, the baseline when an IdP
	// emits no usable groups claim. Subject is "" for this type.
	CapabilitySubjectAll CapabilitySubjectType = "all"
	// CapabilitySubjectUserType is everyone of one user type (UserType.ID); a
	// person holds exactly one, stamped at sign-in. It sits between group and
	// all for governance ceilings/drives; for grants it's one more subject,
	// so a DENY on a type is a wall no user or group allow lifts.
	CapabilitySubjectUserType CapabilitySubjectType = "user_type"
)

// Valid rejects a garbage subject type at the API write boundary, mirroring
// ApprovalScope.Valid.
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

// CapabilityGrant is one row of the permissioning grant list: "subject S may
// (or may not) use capability C at value V".
//
// Capability is a PLAIN STRING, not an enum, deliberately: the closed kind
// set lives in one Go slice (internal/api's capabilityKinds) and is checked
// at the write boundary, so a new kind needs no schema change. An unknown
// kind is simply inert.
//
// Value's meaning is per kind: host or "*.suffix" for egress_host, else an
// exact secret name/workspace uuid/image ref; "*" is always the wildcard.
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
