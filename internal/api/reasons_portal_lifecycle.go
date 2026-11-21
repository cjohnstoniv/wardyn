// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Continues reasons.go's closed set (see its header comment), kept in its own file so reasons_routes.go stays
// under scripts/check-file-size.sh's 1000-line gate.
//
// The suspension, reactivation and purge of a person an issuer portal introduced, and the two reads that find
// one (person_suspend.go, person_purge.go, portal_lifecycle.go).
const (
	reasonPortalPersonOnly                = "portal_person_only"                  // suspend, reactivate, purge or a portal read named a person who is not a portal person
	reasonPortalIssuerNotBound            = "portal_issuer_not_bound"             // the lookup's issuer has no binding (a mis-spelling of a bound one included), or a portal person's issuer lost its binding
	reasonPersonNotFound                  = "person_not_found"                    // no identity of that portal person exists: never seen through any portal, so there is nothing to act on
	reasonPersonNotSuspended              = "person_not_suspended"                // a purge named a person who is neither suspended nor already purged
	reasonPersonPurged                    = "person_purged"                       // suspend or reactivate named a person whose purge is permanent
	reasonPortalLifecycleIncomplete       = "portal_lifecycle_incomplete"         // the suspension or purge is recorded and some of its steps are still pending; retry
	reasonPortalPurgeDeadlineOperatorOnly = "portal_purge_deadline_operator_only" // a suspend asked for a purge deadline and the caller is not a super admin
	reasonPortalPersonParamInvalid        = "portal_person_param_invalid"         // the lookup lacks issuer or person_id, or purge_after_seconds is negative or too large
)
