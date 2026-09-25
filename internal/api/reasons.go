// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The machine-readable refusal reasons the credential-injection lanes (Azure
// DevOps, AWS SSO, Bedrock bearer) send on the wire alongside their human
// sentence — internal/api's half of client.APIError.Reason (#204, #656).
// docs/sdk.md documents this set. By convention a lane's refusal picks one of
// these rather than an ad-hoc string, and a lane that shares a SHAPE with
// another — a hold gone terminal, an approval store that could not be read —
// shares its reason too, instead of inventing a lane-local synonym. Nothing
// enforces the convention yet: the fail() closures take a plain string, and
// the Azure DevOps redemption passes ADOEntraFailure's own values
// (ado_entra_store.go) through unchanged. #656's guard-test acceptance item
// is where that check lands.
//
// Reason coverage is being swept lane by lane (#656); this set grows as
// each lane converts. A reason only one lane currently sends still lives
// here, not beside its lane, so the next lane converted picks from the same
// vocabulary rather than starting a second one.
const (
	// Shared by every lane's resolve arm: the dispatch-time-snapshot family
	// of refusals (I1-I3 in the Azure DevOps and AWS SSO doc comments).
	reasonMissingScopeSnapshot = "missing_scope_snapshot" // the grant names no dispatch-time snapshot
	reasonOwnerNotCaller       = "owner_not_caller"       // the snapshot's owner is not the run token's own subject
	reasonRosterUnreadable     = "roster_unreadable"      // the site configuration could not be read
	reasonScopeChanged         = "scope_changed"          // the live roster has drifted from the dispatch-time snapshot
	reasonRunUnreadable        = "run_unreadable"         // the run row itself could not be read
	reasonStoreError           = "store_error"            // the credential store read failed

	// Azure DevOps resolve only.
	reasonHostNotOrganisation = "host_not_organisation" // the requested host is outside the snapshot's organisation
	reasonTokenMode           = "token_mode"            // dispatch chose the bearer-key lane, not per-user Entra
	reasonSigninUnconfigured  = "signin_unconfigured"   // no Entra app registration for this organisation
	reasonSigninUnreadable    = "signin_unreadable"     // the Entra roster row could not be read

	// AWS SSO resolve only.
	reasonSSOHostNotPortal = "sso_host_not_portal" // the requested host is outside the credential's own SSO portal

	// Bedrock bearer resolve only.
	reasonPerUserBearerAbsent = "per_user_bearer_absent" // the roster names a per-user bearer this owner has none of
	reasonBearerAbsent        = "bearer_absent"          // no bedrock-api-key secret is in the store

	// The Azure DevOps capability-escalation chain and the sign-in/consent
	// HOLD chain — shared with AWS SSO's re-auth hold below, because both
	// are the same shape: an approval-backed hold that can go terminal, hit
	// its per-run cap, or fail to even raise.
	reasonCapabilityNotGrantable   = "capability_not_grantable"
	reasonApprovalsUnreadable      = "approvals_unreadable"
	reasonCapabilityAboveCeiling   = "capability_above_ceiling"
	reasonApprovalMismatch         = "approval_mismatch"
	reasonCapabilityDenied         = "capability_denied"
	reasonCapabilityClosed         = "capability_closed"
	reasonCapabilityAlwaysDeny     = "capability_always_deny"
	reasonCapabilityHoldsExhausted = "capability_holds_exhausted"
	reasonRaiseFailed              = "raise_failed"
	reasonCapabilityReview         = "capability_review"
	reasonOnceUnspendable          = "once_unspendable"

	// The hold-chain terminal/exhausted pair, shared by the Azure DevOps
	// sign-in hold and the AWS SSO re-auth hold — see reasonRaiseFailed and
	// reasonApprovalsUnreadable above for the other two members of this
	// shape.
	reasonSigninClosed         = "signin_closed"
	reasonSigninHoldsExhausted = "signin_holds_exhausted"
)
