// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Terminal copy of `wardyn runner setup` (design ATTACH 11.4): every line it prints is here, so a copy change
// is one edit. Errors carry no "wardyn: " prefix; main adds it.
const (
	// Progress, in step order.
	copySetupRunnerd    = "Using wardyn-runnerd from %s (SHA-256 %s, the one this CLI pins)." // source, hash
	copySetupPackage    = "Using the installed package's wardyn-runnerd, %s."                 // resolved path
	copySetupRegistered = "This computer is already registered with %s, so no registration token was minted."
	copySetupMinted     = "Minted a single-use registration token for this computer (it expires %s); it goes to wardyn-runnerd over a pipe and is never written down." // local time
	copySetupClaimed    = "Claimed runner %s with fingerprint %s."
	copySetupWaiting    = "Waiting up to %s for the runner to connect."
	copySetupConnected  = "Runner %s is connected."
	// The step-up line printed under the server's own self_host_runner refusal.
	copySetupStepUp = "If you are a Wardyn admin: run `wardyn login --admin` (1 hour), then this command again."
	// The restore line (design ATTACH 11.4 Upgrade); copySetupOldVersion stands in when the old version is not known.
	copySetupRestore    = "To go back to %s: `%s setup --org %s`" // old version, the .previous path, org URL
	copySetupOldVersion = "the previous version"
	copySetupLastError  = "wardyn-runnerd's last error: %s"

	// Refusals.
	errSetupWindows       = "runners run in WSL2: run this inside your WSL2 distribution"
	errSetupAdminToken    = "WARDYN_ADMIN_TOKEN cannot mint a runner registration token for you: unset it, then sign in with `wardyn login` or set WARDYN_TOKEN to your personal API token"
	errSetupNoCredential  = "no credential to register this computer with: %s" // noSignInLine
	errSetupUnreachable   = "could not reach %s: %v"
	errSetupOtherOrg      = "this computer is already a runner for %s, not %s: run `wardyn runner setup --url %s`, or revoke that runner and move %s out of the way first" // other URL, this URL, other URL, state dir
	errSetupOtherOrgName  = "another organisation"
	errSetupNoPin         = "this wardyn is a development build and pins no wardyn-runnerd hash, so it cannot check one: use a release wardyn, or run `wardyn-runnerd setup --org %s --token-file <file>` yourself"
	errSetupHashMismatch  = "%s has SHA-256 %s, but this wardyn pins %s: it was not used. Pass --runnerd <file> with the wardyn-runnerd of version %s, or remove that copy" // source, got, pinned, version
	errSetupDownload      = "could not download wardyn-runnerd from %s: %v. Pass --runnerd <file>, or --artifact-base <url> for a mirror"
	errSetupRunnerdFail   = "wardyn-runnerd setup did not finish (%v); its message is above. Run `wardyn runner setup` again once it is fixed: a computer that registered is not registered again"
	errSetupClaimLeft     = "this computer is registered but not claimed: %v; run `wardyn runner setup` again to finish (an unclaimed runner lapses %d hours after it registered)" // types.RunnerUnclaimedTTL
	errSetupNotConnected  = "the runner did not connect within %s"
	errSetupDefaultNoPool = "--default needs --pool"
	errSetupTokenLeft     = "; the token minted for this run expires %s if it was not used" // the mint answer's expires_at, local time
	errSetupNoIdentity    = "it left no runner identity for this organisation"
	errSetupBadIdentity   = "read %s: %w"
	// Download failures, inside errSetupDownload.
	errSetupRedirect  = "refused a redirect to %s, which is not https://"
	errSetupAnswered  = "the server answered %s"
	errSetupRedirects = "more than five redirects"
)
