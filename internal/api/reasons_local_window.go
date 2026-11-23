// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

// The local window's reasons (run_local_window.go): the same closed set and convention as reasons.go, in a file of
// its own beside the code. TestReasonDocsMatchReasonsGo reads this file too.
const (
	reasonLocalWindowSiteOff         = "local_window_site_off"          // features.local_window is not on: no local window on this install
	reasonLocalWindowKind            = "local_window_kind"              // the run is not a Linux VM run
	reasonLocalWindowNotYourComputer = "local_window_not_your_computer" // the run is not on a runner the caller registered as their own
	reasonLocalWindowRecording       = "local_window_recording"         // the run's recording level is Screen and actions, and a local window isn't recorded
	reasonLocalWindowOffline         = "local_window_offline"           // the run's runner is not connected to this server
	reasonLocalWindowRunnerTooOld    = "local_window_runner_too_old"    // the runner's caps say it can't open a local window
	reasonLocalWindowPaused          = "local_window_paused"            // the run is paused, or its presence could not be recorded
	reasonLocalWindowPreparing       = "local_window_preparing"         // the run is still being prepared
	reasonLocalWindowOpen            = "local_window_open"              // the run already has a window open
	reasonLocalWindowDesktopControl  = "local_window_desktop_control"   // a browser desktop that had control of the run did not close in time
	reasonLocalWindowNonce           = "local_window_nonce"             // the runner refused the window's nonce: unknown, expired, or for another run
	reasonLocalWindowFailed          = "local_window_failed"            // the runner could not open the window
)
