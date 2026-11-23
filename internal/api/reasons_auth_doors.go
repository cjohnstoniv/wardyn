// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Continues reasons_routes.go (reasons.go's closed set): the reasons of http.go's auth middlewares, the UI
// gateway with its inbound arm and app ticket, device federation and the harness credential refusal.
// Split out of reasons_routes.go for scripts/check-file-size.sh.
// TestReasonDocsMatchReasonsGo and TestNoAdHocErrorBodyOrReasonLiteral read this file through reasonsGoFiles.

// http.go's own auth middlewares — local mode's loopback/CSRF gates, and the
// admin/run/sensor bearer-token chains. Every value here is already the exact
// string its own auditAuthFailed(As) call wrote before #656 slice 3 put it on
// the wire too, so an operator correlating a 401 to its audit row sees the
// same word twice.
const (
	reasonLocalModePeerNotLoopback = "local_mode_peer_not_loopback"
	reasonLocalModeHostNotLoopback = "local_mode_host_not_loopback"
	reasonAdminTokenNotConfigured  = "admin_token_not_configured"
	reasonMissingBearerToken       = "missing_bearer_token"
	reasonInvalidAdminToken        = "invalid_admin_token"
	reasonAdminTokenRateLimited    = "admin_token_rate_limited" // too many failed admin token guesses from this address (429, Retry-After: 10)
	// reasonIdentityProviderNotConfigured is shared by internalAuth and
	// internalAuthGroundtruth: the identical cause (no embedded identity
	// provider wired) refuses both the run-token and the sensor-token chains.
	reasonIdentityProviderNotConfigured = "identity_provider_not_configured"
	reasonMissingRunToken               = "missing_run_token"
	reasonInvalidRunToken               = "invalid_run_token"
	reasonMissingSensorToken            = "missing_sensor_token"
	reasonInvalidSensorToken            = "invalid_sensor_token"
)

// The UI gateway (uigateway.go, uigateway_session.go): the second,
// un-authenticated-by-session origin that serves a sandbox's own web app.
// uiDialError carries one of these from the dial/launch path back out to
// uiErrorHandler, which is the ONE place that writes the HTTP response for
// that whole path.
const (
	reasonUIGatewayNotFound          = "ui_gateway_not_found"            // no path matched the gateway's own router
	reasonUIGatewayTicketQueryOnPost = "ui_gateway_ticket_query_on_post" // a POST /__wardyn/enter carried the ticket as a query param
	reasonUIGatewayInvalidFormBody   = "ui_gateway_invalid_form_body"    // the POST form body did not parse
	reasonUIGatewayMethodNotAllowed  = "ui_gateway_method_not_allowed"   // /__wardyn/enter saw a method other than GET/POST
	reasonUIGatewayInvalidRunID      = "ui_gateway_invalid_run_id"       // the run field is missing or not a UUID
	reasonUIGatewayWrongHost         = "ui_gateway_wrong_host"           // this run's apps are pinned to a different origin (host mode)
	// reasonUIGatewayTicketInvalid is shared by "not bound to this browser"
	// and "invalid/expired/already-used" — the SAME refusal, byte for byte,
	// so a probe cannot tell binding failure from a genuinely bad ticket.
	reasonUIGatewayTicketInvalid      = "ui_gateway_ticket_invalid"
	reasonUIGatewayTicketLookupFailed = "ui_gateway_ticket_lookup_failed"
	// reasonUIGatewayTicketRunMismatch is shared by "the run could not be
	// read" and "the ticket's stamped principal is not this run's owner" —
	// the identical existence-oracle-safe shape getRunAuthorized's own foreign
	// vs missing parity uses, so a probe cannot tell a missing run from one it
	// does not own.
	reasonUIGatewayTicketRunMismatch          = "ui_gateway_ticket_run_mismatch"
	reasonUIGatewayNotRunning                 = "ui_gateway_not_running"                  // the run is not RUNNING, or has no sandbox
	reasonUIGatewayRunKept                    = "ui_gateway_run_kept"                     // the run is kept (ended/lost); its agent is stopped
	reasonUIGatewayPolicyLookupFailed         = "ui_gateway_policy_lookup_failed"         // the run's effective UI-apps policy could not be read
	reasonUIGatewayAppNotDeclared             = "ui_gateway_app_not_declared"             // the named app is not in the run's policy ui_apps
	reasonUIGatewayNoSession                  = "ui_gateway_no_session"                   // no valid UI relay session cookie for this run/app
	reasonUIGatewaySessionDestinationMismatch = "ui_gateway_session_destination_mismatch" // the dial address does not match the session's run/port
	reasonUIGatewayNoRunner                   = "ui_gateway_no_runner"                    // this deployment configures no runner
	reasonUIGatewayResumeFailed               = "ui_gateway_resume_failed"                // thawing a paused run for the relay failed
	reasonUIGatewayConnCapReached             = "ui_gateway_conn_cap_reached"             // this run already holds the maximum number of open UI connections
	reasonUIGatewayExecFailed                 = "ui_gateway_exec_failed"                  // starting the relay's own exec session failed
	reasonUIGatewayLauncherExecFailed         = "ui_gateway_launcher_exec_failed"         // starting the in-sandbox launcher probe failed
	reasonUIGatewayLauncherMissing            = "ui_gateway_launcher_missing"             // the image has no launcher script for this app
	reasonUIGatewayLauncherNotListening       = "ui_gateway_launcher_not_listening"       // the launcher ran but nothing bound the app's port in time, or an application run's startup command is not listening on the port
	reasonUIGatewayLauncherProbeFailed        = "ui_gateway_launcher_probe_failed"        // the launcher probe exited with an unrecognized code
	reasonUIGatewayConnectionClosed           = "ui_gateway_connection_closed"            // the sandbox closed the relay connection unexpectedly
	reasonUIGatewayBindMissingTicket          = "ui_gateway_bind_missing_ticket"          // POST /__wardyn/bind's form carried no ticket
	// reasonUIGatewayBind{NotSameSite,OriginNotConsole} are uiBindRefusal's own
	// closed set (the ui.authorize/denied row's stable identifiers) — moved
	// here, not left beside it, so this guard can see them (#656 slice 3).
	reasonUIGatewayBindNotSameSite      = "bind_not_same_site"
	reasonUIGatewayBindOriginNotConsole = "bind_origin_not_console"
)

// The UI gateway's inbound arm (uigateway_inbound.go): a call to an app declared inbound: true, and the
// host-mode cross-site check every cookie request passes.
const (
	reasonInboundCredentialInvalid = "inbound_credential_invalid" // the bearer is not a live inbound token (401, WWW-Authenticate: Bearer)
	reasonInboundNotFound          = "inbound_not_found"          // no run, no such app, an app that is not inbound, or a token holder who is not one of its callers: one answer
	reasonInboundRateLimited       = "inbound_rate_limited"       // past the run's or the caller's request rate (429, Retry-After: 1; the inbound.deny row's limit says which)
	reasonUIGatewayCrossSite       = "ui_gateway_cross_site"      // host mode: a session request from another site that is not a top-level navigation, or naming another Origin
)

// An app's ticket (inbound_ticket.go): POST /runs/{id}/apps/{app}/ticket.
const (
	reasonInboundNeedsHostMode = "inbound_needs_host_mode" // a named caller who is not the owner, on a deployment serving apps from one shared host (403)
)

// Device federation (devices_auth.go): the forwarder's own auth chain and
// audit-ingest door. Most values here are already the exact strings
// s.auditAuthFailedAs/auditEnrolFailure/auditIngestFailure wrote before #656
// slice 3 put them on the wire too.
const (
	reasonDeviceStoreUnavailable     = "device_store_unavailable" // this deployment configures no device store (deviceAuth)
	reasonMissingDeviceToken         = "missing_device_token"
	reasonInvalidDeviceToken         = "invalid_device_token"
	reasonDeviceLookupFailed         = "device_lookup_failed"         // reading the device row failed (transient)
	reasonDeviceEnrolRateLimited     = "device_enrol_rate_limited"    // too many enrolment attempts from this address
	reasonDeviceEnrolmentUnavailable = "device_enrolment_unavailable" // handleDeviceEnrol's own no-store case
	reasonDeviceEnrolTokenRequired   = "device_enrol_token_required"  // the enrolment body named no token
	reasonInvalidEnrolmentToken      = "invalid_enrolment_token"
	reasonDeviceIngestInFlight       = "device_ingest_in_flight" // a push from this device is already being processed
	reasonDeviceIngestInvalidBody    = "invalid_body"
	reasonDeviceIngestBatchTooLarge  = "batch_too_large"
	reasonDeviceIngestInvalidRow     = "invalid_row"
	reasonDeviceIngestOrgRun         = "org_run"
	reasonDeviceIngestChainMismatch  = "chain_mismatch"
	// /api/v1/admin/devices/enrolment-tokens (devices.go).
	reasonDeviceEnrolmentTokenNameInvalid = "device_enrolment_token_name_invalid"
)

// The harness.credential.refuse row's own FIXED reason vocabulary
// (refuseCapture, awssso_pin.go): a closed set on purpose, since the
// alternative is sandbox-chosen text an incident review cannot group by.
// Moved here from beside refuseCapture (#656 slice 3) so the docs guard,
// which only reads this file, can see them now that refuseCapture's reason
// also reaches the wire.
const (
	reasonCaptureBlobShape        = "blob_shape"
	reasonCaptureFieldUnsafe      = "field_unsafe"
	reasonCaptureFieldShape       = "field_shape"
	reasonCaptureRegionMismatch   = "region_mismatch"
	reasonCaptureStartURLMismatch = "start_url_mismatch"
	reasonCaptureAccountRolePin   = "account_role_pin_mismatch"
	reasonCaptureModelAccount     = "model_account_mismatch"
	reasonCaptureUnstampedScope   = "unstamped_scope"
	reasonCaptureAlreadyCaptured  = "already_captured"
	reasonCaptureStampUnreadable  = "stamp_unreadable"
	// reasonCaptureRunKilled: the login run this upload comes from has been
	// KILLED — by its own Cancel, or by the person's next sign-in superseding
	// it (harnesscred_supersede.go). See ssoTokenRunKilledRefusal.
	reasonCaptureRunKilled = "run_killed"
	// reasonCaptureProviderChanged: a sign-in through a model provider's own
	// door whose provider was removed, re-kinded or re-addressed while the
	// login sandbox was open (storeProviderSignIn).
	reasonCaptureProviderChanged = "provider_changed"
	// reasonCaptureSignInBusy: the per-person sign-in lock could not be taken
	// in time (lockLoginSupersede), so the capture was not serialized and is
	// refused rather than stored.
	reasonCaptureSignInBusy = "signin_busy"
)
