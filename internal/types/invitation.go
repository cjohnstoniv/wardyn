// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// InvitationState is where an invitation stands. It is derived from the row and the clock, never stored.
type InvitationState string

const (
	InvitationPending InvitationState = "pending"
	InvitationClaimed InvitationState = "claimed"
	InvitationExpired InvitationState = "expired"
	InvitationRevoked InvitationState = "revoked"
)

// Invitation lets one named person mint their own API token (migration 0157, #1508). It holds no
// secret: only the target, signed in, can claim it, and TokenID is metadata that authenticates nothing.
type Invitation struct {
	ID              uuid.UUID       `json:"id"`
	Target          string          `json:"target"`
	CreatedBy       string          `json:"created_by"`
	CreatedAt       time.Time       `json:"created_at"`
	ExpiresAt       time.Time       `json:"expires_at"`
	TokenName       string          `json:"token_name,omitempty"`
	TokenTTLSeconds *int64          `json:"token_ttl_seconds,omitempty"` // nil = the deployment rule
	ReplacesTokenID *uuid.UUID      `json:"replaces_token_id,omitempty"`
	ClaimedAt       *time.Time      `json:"claimed_at,omitempty"`
	TokenID         *uuid.UUID      `json:"token_id,omitempty"`
	RevokedAt       *time.Time      `json:"revoked_at,omitempty"`
	RevokedBy       string          `json:"revoked_by,omitempty"`
	State           InvitationState `json:"state"`
	ConsolePath     string          `json:"console_path,omitempty"` // create response only
}

// StateAt is the invitation's state at now: claimed and revoked are final, then the clock decides.
func (i Invitation) StateAt(now time.Time) InvitationState {
	switch {
	case i.ClaimedAt != nil:
		return InvitationClaimed
	case i.RevokedAt != nil:
		return InvitationRevoked
	case !i.ExpiresAt.After(now):
		return InvitationExpired
	}
	return InvitationPending
}
