// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// TokenDisposition is where an admin-minted token stands (a token whose minter is not its owner). It is
// derived at read from the token, the invitations that name it and its move row, never stored (#1508).
type TokenDisposition string

const (
	// TokenNeedsDecision: live, with no pending invitation replacing it and no move. Nothing retires it.
	TokenNeedsDecision TokenDisposition = "needs_decision"
	// TokenAwaitingOwner: live, and a pending invitation to its owner names it as the token it replaces.
	TokenAwaitingOwner TokenDisposition = "awaiting_owner"
	// TokenReplacedByOwner: revoked by the claim of the invitation that replaced it.
	TokenReplacedByOwner TokenDisposition = "replaced_by_owner"
	// TokenLinkedToService: live, and an admin linked it to the service identity that replaces it.
	TokenLinkedToService TokenDisposition = "linked_to_service_identity"
	// TokenMovedToService: revoked, with a move row.
	TokenMovedToService TokenDisposition = "moved_to_service_identity"
	// TokenRevoked: revoked, with neither a claim nor a move behind it.
	TokenRevoked TokenDisposition = "revoked"
)

// TokenMove is the row POST /tokens/{id}/move writes: an admin-minted token linked to the service identity
// that replaces it. The token stays live until an admin revokes it.
type TokenMove struct {
	TokenID         uuid.UUID `json:"token_id"`
	ServiceIdentity string    `json:"service_identity"`
	LinkedBy        string    `json:"linked_by"`
	LinkedAt        time.Time `json:"linked_at"`
}

// TokenMoved is the answer to POST /tokens/{id}/move.
type TokenMoved struct {
	TokenMove
	Disposition TokenDisposition `json:"disposition"`
}
