// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Governance change states (migration 0120's CHECK).
const (
	GovernanceChangePending  = "pending"
	GovernanceChangeApplied  = "applied"
	GovernanceChangeRejected = "rejected"
	GovernanceChangeExpired  = "expired"
	GovernanceChangeStale    = "stale"
)

// The target kinds a governance change can hold (governance_changes.target_kind has no CHECK: the
// closed set is the table in internal/api, and a later lane adds a kind with no DDL).
const (
	GovernanceTargetProfile    = "governance_profile"
	GovernanceTargetAssignment = "governance_assignment"

	GovernanceTargetCapabilityGrant        = "capability_grant"
	GovernanceTargetCapabilityEnforcement  = "capability_enforcement"
	GovernanceTargetCapabilityAvailability = "capability_availability"
	GovernanceTargetUserTypePriority       = "user_type_priority"
	GovernanceTargetRoleMapping            = "role_mapping"
)

// GovernanceChange is one governance write held for a second human (migration 0120). Payload is the
// validated request, replayed on approval; Diff is the server-rendered before/after the reviewer
// reads. Neither carries a secret value.
//
// The four personal fields (ProposedBy, ProposedByEmail, DecidedBy, DecidedByEmail) are cleared by the
// audit_personal_fields erasure scope. The emails never reach a response: they exist for the
// distinct-human comparison and the erasure match only.
type GovernanceChange struct {
	ID         uuid.UUID       `json:"id"`
	TargetKind string          `json:"target_kind"`
	Op         string          `json:"op"`
	TargetKey  string          `json:"target_key"`
	State      string          `json:"state"`
	ProposedBy string          `json:"proposed_by"`
	ProposedAt time.Time       `json:"proposed_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Diff       json.RawMessage `json:"diff"`
	DecidedBy  string          `json:"decided_by,omitempty"`
	DecidedAt  *time.Time      `json:"decided_at,omitempty"`
	// Reason is a rejection's optional explanation.
	Reason string `json:"reason,omitempty"`

	// Payload, hashes and emails stay server-side.
	Payload         json.RawMessage `json:"-"`
	BaseHash        string          `json:"-"`
	DeploymentHash  string          `json:"-"`
	ProposedByEmail string          `json:"-"`
	DecidedByEmail  string          `json:"-"`
}
