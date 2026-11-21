// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Continues reasons.go's closed set (see its header comment), kept in its own file so reasons_routes.go stays
// under scripts/check-file-size.sh's 1000-line gate.
//
// Portal exchange (delegation_portal.go). The rulings on admin-written person lists come from the store
// writes; the rest name why an issuer portal's exchange refused or could not decide.
const (
	reasonDelegatePersonNotAllowed    = "delegate_person_not_allowed"    // a person list that confers an admin, sponsor, approver or console role named a portal person
	reasonDelegatePersonUnknownPortal = "delegate_person_unknown_portal" // a person list named a portal person whose issuer has no binding
	reasonPortalIssuerConflict        = "portal_issuer_conflict"         // the exchange: the portal's issuer is this process's own console issuer
	reasonPortalAdmissionChanged      = "portal_admission_changed"       // the exchange: the portal's admission rule changed after the token was checked; retry
	reasonActorTokenUnsupported       = "actor_token_unsupported"        // the exchange: an issuer portal takes no actor token
	reasonPortalRegistrationInvalid   = "portal_registration_invalid"    // the exchange: the portal's own row cannot be verified against (no client id, or an access profile with no audience or scopes)
	reasonPortalUnavailable           = "portal_unavailable"             // the exchange: the portal's stores, verifier or issuer set are not available on this server
)
