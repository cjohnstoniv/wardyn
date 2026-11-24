// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The run-token recovery reasons (#4082), split out like reasons_revive.go: the same closed set and the same
// convention as reasons.go, not a second vocabulary. TestReasonDocsMatchReasonsGo and TestNoAdHocAuthz read this
// file too.

// POST /internal/token/recover (internal_token_recover.go): a 403 or 501 is final, and the proxy stops asking;
// every other answer is retryable. The run-state refusals the renew door shares keep its reasons
// (authz.ReasonRunTerminal, authz.ReasonRunKept, reasonRunNotFound, reasonRunContainmentPending,
// reasonRunRenewLeaseBusy).
const (
	reasonRecoveryUnsupported  = "recovery_unsupported"   // 501: this server's identity provider or store cannot recover a token
	reasonTokenNotExpired      = "token_not_expired"      // 409: the token is still valid; renew it instead
	reasonTokenTooOld          = "token_too_old"          // 403: the token expired more than 24 hours ago
	reasonRunRevoked           = "run_revoked"            // 403: the run's tokens were revoked
	reasonRunNotRunning        = "run_not_running"        // 403: the run is not running
	reasonRunPastLifetime      = "run_past_lifetime"      // 403: the run is past its end or its lifetime cap
	reasonRunTokenCut          = "run_token_cut"          // 403: the run's token lineage was cut at a stopped pause; only a revive runs it again
	reasonRunHeldContained     = "run_held_contained"     // 403: the run's runner holds it contained
	reasonStrictAllowanceSpent = "strict_allowance_spent" // 403: the run's strict outage allowance has run out
	reasonTokenLineageRetired  = "token_lineage_retired"  // 503: the presented token is not the run's current one
	reasonOutageWindowPending  = "outage_window_pending"  // 503: no recorded outage covers the token's expiry yet
	reasonRecoveryReadFailed   = "recovery_read_failed"   // 503: a read the recovery needs failed
)

// POST /runs/{id}/revive and the admin bulk restart (run_revive.go reviveEligible, run_outage_hold.go).
const (
	reasonReviveStrictAllowanceSpent = "revive_strict_allowance_spent" // 409: a run lost to an outage whose strict outage allowance has run out
)
