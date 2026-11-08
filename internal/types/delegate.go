// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// Delegate is a registered portal (migration 0094, #1142): a trusted front-end
// that may exchange a signed-in person's own identity-provider token for a
// short delegated token acting for that person. IdPClientID is the portal's
// own client id at the identity provider, which a subject token must name;
// Group is the one group a person must be in for the portal to act for them.
// Revoking the row (RevokedAt) ends every delegated token it minted at once.
type Delegate struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	IdPClientID string    `json:"idp_client_id"`
	Group       string    `json:"group"`
	// Never serialized: a credential hash has no business leaving the process.
	CredentialSHA256 string     `json:"-"`
	RegisteredBy     string     `json:"registered_by"`
	CreatedAt        time.Time  `json:"created_at"`
	LastUsedAt       *time.Time `json:"last_used_at,omitempty"`
	RevokedAt        *time.Time `json:"revoked_at,omitempty"`
	// CutInFlightAt is when a revoke also cut the gateway access of the runs this portal
	// launched (DELETE /admin/delegates/{id}?cut_in_flight=true): from then on no run it
	// launched gets a gateway token. nil, the default, leaves those runs their access.
	CutInFlightAt *time.Time `json:"cut_in_flight_at,omitempty"`
	// RunInUnder pins the portal to one governance unit, set at registration only: a run launched
	// through it must resolve to this unit or one below it. nil, the default, is unpinned. Stored
	// without a foreign key, so a deleted unit leaves a pin that refuses every launch.
	RunInUnder *uuid.UUID `json:"run_in_under,omitempty"`
	// Credential is the plaintext portal credential, on the register
	// response ONLY; the row stores its hash.
	Credential string `json:"credential,omitempty"`
}

// DelegatedToken is one live token a Delegate was handed for one person: the
// identity it replays (always at the user role — a portal never carries admin
// reach) and when it stops working. Stored only by hash, never refreshed.
type DelegatedToken struct {
	ID              uuid.UUID `json:"id"`
	DelegateID      uuid.UUID `json:"delegate_id"`
	Principal       string    `json:"principal"`
	Email           string    `json:"email,omitempty"`
	UserType        string    `json:"user_type"`
	Groups          []string  `json:"groups"`
	GroupsTruncated bool      `json:"groups_truncated"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	// RevSeq is the revocation sequence read when the token was minted; 0 for a token minted before it
	// existed, which a revocation cutoff orders on CreatedAt. Never on the wire.
	RevSeq int64 `json:"-"`
	// Actor is the agent the portal named as acting for the person on the exchange (actor_token),
	// verified against the deployment's own identity provider; nil when the portal named none.
	Actor *DelegationActor `json:"actor,omitempty"`
}

// DelegationVia names the portal and the delegated token an act was performed
// through. It is the audit row's data.via and an attach ticket's replayed
// origin; nil everywhere a person acted for themselves.
type DelegationVia struct {
	Delegate uuid.UUID `json:"delegate"`
	Grant    uuid.UUID `json:"grant"`
	// Actor is the delegated token's acting agent. omitempty: a token minted without one writes
	// exactly the via it always did.
	Actor *DelegationActor `json:"actor,omitempty"`
}

// DelegationActor names the agent acting for a person through a portal: the subject and issuer of the
// token the identity provider issued to the portal for it. It is recorded, never authorised: the person
// stays the owner and the actor of everything the delegated token does.
type DelegationActor struct {
	Sub string `json:"sub"`
	Iss string `json:"iss"`
}
