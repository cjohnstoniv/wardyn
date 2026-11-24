// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Continues reasons.go's closed set (see its header comment) with the model provider service grant reasons,
// kept in their own file so reasons_routes.go stays under scripts/check-file-size.sh's 1000-line gate.
const (
	reasonServiceGrantUnavailable            = "service_grant_unavailable"              // the token request did not complete, or the store could not be read; retry
	reasonServiceGrantRefused                = "service_grant_refused"                  // the identity provider or the stored record refused the grant; an administrator must check it
	reasonServiceGrantFailing                = "service_grant_failing"                  // a live server's latest token request for this provider failed (setup status cause)
	reasonServiceGrantNotListed              = "service_grant_not_listed"               // the service identity is not one the grant lists
	reasonServiceGrantNotConfigured          = "service_grant_not_configured"           // the provider has no service grant
	reasonServiceGrantDestinationChanged     = "service_grant_destination_changed"      // the body's token URL, client id or gateway address is not the provider's
	reasonServiceGrantsNotActivated          = "service_grants_not_activated"           // service grants wait for the 0.10.2 switch-on
	reasonServiceGrantCleanupPending         = "service_grant_cleanup_pending"          // a removed grant's secret is still to be deleted under this provider
	reasonServiceGrantCleanupNotFound        = "service_grant_cleanup_not_found"        // no clean-up names that uid
	reasonServiceGrantSecretKept             = "service_grant_secret_kept"              // the secret's deletion did not finish; it stays listed and is retried
	reasonServiceGrantCleanupRevisionChanged = "service_grant_cleanup_revision_changed" // the record was written outside Wardyn after the removal; it needs reconciling
	reasonServiceGrantCleanupUnsupported     = "service_grant_cleanup_unsupported"      // the secret store keeps no revision, so nothing is deleted
	reasonServiceGrantSecretChanged          = "service_grant_secret_changed"           // the secret record changed while its delete ran; it is kept
)
