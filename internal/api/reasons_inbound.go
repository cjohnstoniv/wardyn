// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Inbound federation (inbound_federation.go, inbound_exchange.go): an organization's own provider token swapped for a
// short-lived inbound token of one service identity (#3787).
const (
	reasonInboundFederationOff         = "inbound_federation_off"          // 404: the switch is off, the store keeps no trusts, or local mode
	reasonInboundExchangeRateLimited   = "inbound_exchange_rate_limited"   // 429: this address spent its exchange attempts
	reasonInboundSubjectTokenInvalid   = "inbound_subject_token_invalid"   // 401: one answer for every refused subject token
	reasonInboundIssuerUnavailable     = "inbound_issuer_unavailable"      // 503: the trusted provider could not be reached; Retry-After
	reasonInboundTrustInvalid          = "inbound_trust_invalid"           // 400: a trust field is missing, malformed or out of range
	reasonInboundTrustReserved         = "inbound_trust_reserved"          // 400: an issuer or audience a trust may never name
	reasonInboundTrustExists           = "inbound_trust_exists"            // 409: that name, or that issuer, audience and subject, is taken
	reasonInboundTrustNotFound         = "inbound_trust_not_found"         // 404: no such trust, or not on an identity you sponsor
	reasonInboundTrustUnconfirmed      = "inbound_trust_unconfirmed"       // 409: the sponsor was revoked after the trust's last confirm
	reasonInboundFederationNeedsPerson = "inbound_federation_needs_person" // 403: switch on, trust create or confirm by a token
)
