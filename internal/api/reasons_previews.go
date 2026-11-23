// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// Preview URLs' reasons: the same closed set and convention as reasons.go, in a file of their own beside
// run_previews.go. TestReasonDocsMatchReasonsGo reads this file too. A preview's background refusal is
// run_background_only, and an unreadable data_out rule is data_out_unreadable, as at every web app's door.
const (
	reasonPreviewURLsOff       = "preview_urls_off"        // features.preview_urls is off, or this release cannot turn it on: no preview is created or opened
	reasonPreviewConsoleOnly   = "preview_console_only"    // a preview is created, and an org preview opened, by a person signed in to the console only
	reasonPreviewInvalid       = "preview_invalid"         // the body did not decode, or its visibility or ttl_seconds is out of range
	reasonPreviewAboveCeiling  = "preview_above_ceiling"   // the visibility is wider than features.preview_urls.max_visibility
	reasonPreviewNotRunning    = "preview_not_running"     // the run is not running
	reasonPreviewProfile       = "preview_profile"         // the run's profile denies UI apps or interactive sessions
	reasonPreviewPortReserved  = "preview_port_reserved"   // the port is not one from 1024 to 65535, or is the run's proxy port
	reasonPreviewPortDeclared  = "preview_port_declared"   // the port is one of the run's declared web apps
	reasonPreviewDataOut       = "preview_data_out"        // the run's data_out rule is review or blocked
	reasonPreviewNeedsHostMode = "preview_needs_host_mode" // an org or public preview on a deployment that serves apps from one host
	reasonPreviewRunPaused     = "preview_run_paused"      // the gateway: a preview of a paused run, which a preview never wakes
	reasonPreviewNotListening  = "preview_not_listening"   // the gateway: nothing listens on the preview's port in the run yet
)
