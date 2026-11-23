// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"time"

	"github.com/google/uuid"
)

// InboundTrust lets a token from an organization's own identity provider be exchanged for a short-lived inbound
// token of one service identity (#3787). Issuer, audience and subject are matched by exact bytes.
type InboundTrust struct {
	ID                 uuid.UUID `json:"id"`
	Name               string    `json:"name"`
	Issuer             string    `json:"issuer"`
	InternalIssuer     string    `json:"internal_issuer,omitempty"`
	Audience           string    `json:"audience"`
	Subject            string    `json:"subject"`
	RequiredClaim      string    `json:"required_claim,omitempty"`
	RequiredClaimValue string    `json:"required_claim_value,omitempty"`
	ServiceIdentity    string    `json:"service_identity"`
	TokenTTLSeconds    int       `json:"token_ttl_seconds"`
	MaxTokenAgeSeconds int       `json:"max_token_age_seconds"`
	MaxLive            int       `json:"max_live"`
	CreatedAt          time.Time `json:"created_at"`
	CreatedBy          string    `json:"created_by"`
	// ConfirmedAt and ConfirmedSeq are where the trust sits against its sponsor's revocations: set at its create and
	// at each confirm by the sponsor. A revoke of the sponsor that draws a greater sequence stops the trust.
	ConfirmedAt  time.Time `json:"confirmed_at"`
	ConfirmedBy  string    `json:"confirmed_by"`
	ConfirmedSeq int64     `json:"-"`
}

// GovernanceTargetInboundFederation is the governance change kind that holds turning inbound federation on and
// adding a trust under WARDYN_GOVERNANCE_SECOND_HUMAN, beside the kinds in governance_change.go.
const GovernanceTargetInboundFederation = "inbound_federation"

// InboundFederationSwitchKey is the target key of a held turn-on. A held trust create's key is the trust's name,
// which never holds '_', so the two never meet.
const InboundFederationSwitchKey = "inbound_federation"

// InboundFederation is the organization's one switch for inbound trusts. Off, nothing is exchanged.
type InboundFederation struct {
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
	UpdatedBy string    `json:"updated_by,omitempty"`
}

// The bounds of an inbound trust, the same as the table's checks.
const (
	InboundTrustTTLMinSeconds        = 60
	InboundTrustTTLMaxSeconds        = 3600
	InboundTrustTTLDefaultSeconds    = 900
	InboundTrustMaxAgeDefaultSeconds = 300
	InboundTrustMaxLiveDefault       = 100
	InboundTrustMaxLiveMax           = 1000
)
