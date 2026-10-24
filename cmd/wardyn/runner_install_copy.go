// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

// Terminal copy of `wardyn runner install` and `wardyn runner upgrade` (#3312). Errors carry no "wardyn: "
// prefix; main adds it.
const (
	copyInstallVerified    = "Verified %s: v%s's SHA256SUMS is signed by %s, and lists SHA-256 %s for it." // URL, version, identity, hash
	copyUpgradeCurrent     = "This computer's runner is already v%s: nothing to do."
	copyUpgradeSwapping    = "Replacing the runner (%s) with v%s and restarting its service."
	copyUpgradeDone        = "The runner is v%s, started and connected."
	copyUpgradeRollingBack = "v%s did not start (%v): putting %s back."
	// The offer, printed after the version notice (design ATTACH 11.7): runner version, organisation release twice.
	copyNoticeRunnerOlder = "this computer's runner is %s and the organisation runs %s: upgrade it with `wardyn runner upgrade --version %s`"

	errInstallVersion        = "%q is not a release version: a development build of wardyn has none, so pass --version <x.y.z>"
	errInstallPlatform       = "no runner is published for %s/%s"
	errInstallManagedBase    = "the managed preference ArtifactBase: %v"
	errInstallAdvertisedBase = "the organisation's advertised mirror (/healthz.cli_artifact_base): %v"
	errInstallSums           = "could not download SHA256SUMS from %s: %v. Pass --artifact-base <url> for a mirror"
	errInstallNoBundle       = "signature bundle missing: %s has no SHA256SUMS.sigstore.json, so v%s cannot be verified and nothing was installed. Releases from before 0.10.0 publish none; a mirror must carry it beside SHA256SUMS" // dir, version
	errInstallUnverified     = "refused the release at %s: %v. Nothing was installed"                                                                                                                                                  // dir, reason
	errInstallNotListed      = "v%s's signed SHA256SUMS lists no %s"
	errInstallAssetMissing   = "asset missing on mirror: %s has no %s for v%s. The mirror must carry this platform's runner beside SHA256SUMS and SHA256SUMS.sigstore.json" // base, asset, version
	errInstallDownload       = "could not download %s: %v"
	errInstallChecksum       = "refused %s: its SHA-256 is %s, but the signed SHA256SUMS lists %s. Nothing was installed" // URL, got, want
	errUpgradeNotInstalled   = "no runner is installed on this computer: run `wardyn runner install`"
	errUpgradeNoOrg          = "the installed runner's service names no organisation: run `wardyn runner install --url <organisation>`"
	errUpgradeNotReplaced    = "v%s did not start (%v), and this upgrade kept no previous runner to put back, so the runner was left as it was"
	errUpgradeRolledBack     = "v%s did not start (%v), so the runner is back on %s"
	errUpgradeRollbackFailed = "v%s did not start (%v), and putting %s back failed too (%v). %s" // version, why, old version, why, restore line
)
