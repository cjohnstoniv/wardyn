// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Terminal copy of `wardyn login`, `whoami` and `logout`, and of the loopback page the browser lands on: every
// line they print is here, so a copy change is one edit. Errors carry no "wardyn: " prefix; main adds it.
const (
	// login, through the browser (board ATT1-Terminal).
	copyOpening    = "Opening %s in your browser."             // the approve URL
	copyCheckCode  = "Check that the page shows this code: %s" // the user code
	copySignedIn   = "Signed in to %s as %s on %s"             // host, person, computer
	copyWhoUnknown = "you"                                     // the person, when GET /api/v1/me did not answer

	// login --no-browser (paste-back).
	copyPasteOpen   = "Open %s in a browser on any computer."
	copyPastePrompt = "Paste the code the page shows: "

	// login --admin.
	copyAdminFirst    = "Admin access needs a CLI sign-in on this computer: signing in first."
	copyAdminSignedIn = "Admin access to %s as %s until %s. It does not refresh." // host, person, local time

	// The file store, on stderr at login: the path, and types.CLIFileStoreSessionCap in words.
	copyFileStore = "wardyn: this computer has no keychain, so the sign-in is kept in %s, which only you can read, and lasts at most %s"

	// The loopback page (boards MATT2-Approve, MATT2-States).
	pageTitle       = "Wardyn CLI"
	pageSignedIn    = "Signed in. You can close this tab and go back to your terminal."
	pageDeniedTitle = "Denied"
	pageDeniedBody  = "Nothing was signed in. You can close this tab."
	pageFailed      = "The sign-in did not finish. Go back to your terminal."

	// login errors.
	errDenied           = "the sign-in was denied in the browser; nothing was saved"
	errNoBrowserNoPaste = "this computer has no browser and your organisation has not turned on sign-in without one (WARDYN_CLI_PASTE_SIGN_IN): sign in on a computer with a browser, or set WARDYN_TOKEN"
	errNoBrowserPaste   = "this computer has no browser: run `wardyn login --no-browser`"
	errNoCallbackPaste  = "the browser did not come back to the CLI within 5 minutes: run `wardyn login --no-browser`"
	errNoCallback       = "the browser did not come back to the CLI within 5 minutes: sign in on a computer with a browser, or set WARDYN_TOKEN"
	errNoPasteMode      = "your organisation has not turned on sign-in without a browser (WARDYN_CLI_PASTE_SIGN_IN): sign in on a computer with a browser, or set WARDYN_TOKEN"
	errNoCode           = "no code was pasted; nothing was saved"
	errNoOrg            = "name your organisation: `wardyn login <url>`"
	errUnreachable      = "saved %s, but could not reach it to sign in: %v"

	// A stored sign-in that can no longer renew. The reason is one of the endReason* values.
	errSessionEnded   = "the CLI sign-in for %s has ended (%s): run `wardyn login` to sign in again"
	endReasonExpired  = "it reached the end of its lifetime"
	endReasonIdle     = "it went unused past its idle lifetime"
	endReasonServer   = "the server refused to renew it: %s" // the server's reason code; it never says which end
	errRenewBusy      = "another wardyn command is renewing the CLI sign-in for %s; retry in a moment"
	errRenew          = "could not renew the CLI sign-in for %s: %v"
	hintStoredRefused = "the CLI sign-in's token was refused: run `wardyn whoami`, or `wardyn login` to sign in again"

	// whoami (board ATT1-Terminal frame 5).
	copyRoleUser   = "user; admin commands need `wardyn login --admin`"
	copyRoleAdmin  = "%s; admin access ends in %s"
	copyWhoStore   = "%s · session ends in %s · idle end in %s" // store, durations
	copyWhoToken   = "token from %s"                            // --token, WARDYN_ADMIN_TOKEN or WARDYN_TOKEN
	errNotSignedIn = "not signed in to %s: run `wardyn login`"

	// The version notice, on stderr (design 11.7; board ATT1-Terminal frame 5): this CLI, the URL, the server.
	copyNoticeServerNewer = "this CLI is %s and %s runs %s: update with the line under Get the CLI"
	copyNoticeServerOlder = "this CLI is %s and %s runs %s, an older release: if a command is refused, install the matching CLI with the line under Get the CLI"

	// logout.
	copyLoggedOut   = "Signed out of %s on this computer."
	copyNotSignedIn = "Not signed in to %s."
	errLogoutServer = "signed out on this computer, but %s did not end the session (%v): sign it out under Your account → Signed-in CLIs"
)
