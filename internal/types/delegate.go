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

	// Issuer portal fields. A console portal has Issuer == "" and every field below at its zero value,
	// so its JSON and its reads are what they were before.
	//
	// Issuer is the exact registered issuer string: the bytes every accepted token's iss carries. A row
	// with one is an issuer portal, its credential has the wdi_ prefix and its grants live in portal_grants.
	Issuer string `json:"issuer,omitempty"`
	// InternalIssuer is the address the server fetches discovery and keys from, as registered.
	InternalIssuer string `json:"internal_issuer,omitempty"`
	// IssuerKey is the 26-character key of the issuer binding; the people of this portal are
	// delegate:person:<IssuerKey>:<e>.
	IssuerKey    string   `json:"issuer_key,omitempty"`
	TokenProfile string   `json:"token_profile,omitempty"`
	GroupsClaim  string   `json:"groups_claim,omitempty"`
	EmailDomains []string `json:"email_domains,omitempty"`
	// UserType is the user type every person of this portal gets, never read from a claim.
	UserType         string   `json:"user_type,omitempty"`
	UISessionRenewal bool     `json:"ui_session_renewal,omitempty"`
	Audience         string   `json:"audience,omitempty"`
	RequiredScopes   []string `json:"required_scopes,omitempty"`
	// MaxSubjectTokenAgeSeconds and GrantTTLSeconds are the freshness bound and the grant lifetime of an
	// issuer portal, 300-600 and 300 up to the former.
	MaxSubjectTokenAgeSeconds int `json:"max_subject_token_age_seconds,omitempty"`
	GrantTTLSeconds           int `json:"grant_ttl_seconds,omitempty"`
	// RequiredClaim is the one optional exact claim match; PortalManagedAdmission says that an empty Group
	// is on purpose.
	RequiredClaim          *PortalRequiredClaim `json:"required_claim,omitempty"`
	PortalManagedAdmission bool                 `json:"portal_managed_admission,omitempty"`
	// AdmissionRev starts at 1 and is incremented by every applied admission change.
	AdmissionRev int64 `json:"admission_rev,omitempty"`
}

// PortalRequiredClaim is one top-level signed claim an issuer portal's token must carry, as a string equal
// to Value byte for byte.
type PortalRequiredClaim struct {
	Claim string `json:"claim"`
	Value string `json:"value"`
}

// PortalAdmission is the part of an issuer portal that decides who it admits: its group (empty with
// PortalManaged), the portal-managed flag and the one claim match.
type PortalAdmission struct {
	Group         string               `json:"group"`
	PortalManaged bool                 `json:"portal_managed_admission"`
	RequiredClaim *PortalRequiredClaim `json:"required_claim,omitempty"`
}

// PortalGrant is one grant an issuer portal handed a person: the wdg_ bearer's row in portal_grants. The
// bearer is stored only by hash. Issuer and IssuerKey are not columns of the grant: the reads that join the
// live portal fill them from the portal's row, for the reverse check (the principal must start with
// delegate:person:<IssuerKey>:).
type PortalGrant struct {
	ID         uuid.UUID `json:"id"`
	DelegateID uuid.UUID `json:"delegate_id"`
	Principal  string    `json:"principal"`
	UserType   string    `json:"user_type"`
	// ChainID joins the grants of one continuing browser-app session of one person through one portal.
	ChainID         uuid.UUID `json:"chain_id"`
	SubjectIssuedAt time.Time `json:"subject_issued_at"`
	// RevSeq is the revocation sequence read before the grant was minted; a cut above it ends the grant.
	RevSeq    int64     `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	// Issuer and IssuerKey come from the joined portal row on a read; ignored on a mint.
	Issuer    string `json:"-"`
	IssuerKey string `json:"-"`
}

// PortalPerson is what the store knows of one portal person from the issuer and the person id.
type PortalPerson struct {
	Principal     string     `json:"principal"`
	IdentityID    uuid.UUID  `json:"identity_id"`
	DisplayEmail  string     `json:"display_email"`
	DeactivatedAt *time.Time `json:"deactivated_at,omitempty"`
	PurgedAt      *time.Time `json:"purged_at,omitempty"`
	// PortalsUsed are the portals that issued this person a grant, with each portal's pin.
	PortalsUsed []PortalUsed `json:"portals_used"`
}

// PortalUsed is one portal a person has used.
type PortalUsed struct {
	Delegate   uuid.UUID  `json:"delegate"`
	Name       string     `json:"name"`
	RunInUnder *uuid.UUID `json:"run_in_under,omitempty"`
	RevokedAt  *time.Time `json:"revoked_at,omitempty"`
}

// PortalIssuer is one issuer binding: the exact issuer spelling, the normalised internal address and the
// claim that names a person. It is bound once and fixed while the issuer has people.
type PortalIssuer struct {
	IssuerKey          string    `json:"issuer_key"`
	Issuer             string    `json:"issuer"`
	InternalIssuerNorm string    `json:"internal_issuer_norm"`
	PersonIDClaim      string    `json:"person_id_claim"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedBy          string    `json:"created_by"`
}

// ConsoleIssuer is a console issuer a replica of this deployment has booted with. Rows are only added.
type ConsoleIssuer struct {
	IssuerNorm           string    `json:"issuer_norm"`
	Issuer               string    `json:"issuer"`
	FirstBootedAt        time.Time `json:"first_booted_at"`
	FirstBootedByReplica string    `json:"first_booted_by_replica"`
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
