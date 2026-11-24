// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package version holds Wardyn's one shipped version string, kept out of
// cmd/* so the CLI, daemon startup log, and /healthz all report the same
// build — needed to diagnose CLI/server skew after a rolling upgrade.
package version

import "runtime/debug"

// Version is the shipped release; cmd/wardyn/version_test.go pins it to the
// newest CHANGELOG.md section and the other shipped version strings
// (deploy/helm/wardyn/Chart.yaml, ui/package.json) — bump them together.
const Version = "0.10.4"

// releaseBuild is stamped "true" via -ldflags -X, only by release.yml's
// binaries job and Dockerfile.wardynd's RELEASE_BUILD arg (never by
// publish-image.yml's per-commit build). Every other build leaves it empty,
// so String() falls back to the commit suffix; this is the sole mechanism
// that detects a release build.
var releaseBuild string

// buildRevision is the commit an image build was made from, stamped via -ldflags -X by the Dockerfile's
// REVISION arg (publish-image.yml's per-commit build). The image build context carries no .git, so the
// toolchain records no vcs.revision of its own; String() falls back to this one. Empty everywhere else.
var buildRevision string

// readBuildInfo is a seam so a test can stand in for the running binary's own
// module info, which only debug.ReadBuildInfo can read.
var readBuildInfo = debug.ReadBuildInfo

// String reports the build an operator is actually running. A release build
// returns the bare Version, byte-exact so the release-cut check in
// version_test.go stays true. Otherwise it appends the build commit and
// "-dirty" if uncommitted (e.g. "0.7.12+80852d67f-dirty"), so a pinned main
// build between releases is identifiable in --version, the startup log, and
// /healthz.
func String() string {
	if releaseBuild == "true" {
		return Version
	}
	var rev string
	var dirty bool
	if info, ok := readBuildInfo(); ok {
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
	}
	if rev == "" {
		rev = buildRevision
	}
	if rev == "" {
		return Version
	}
	if len(rev) > 9 {
		rev = rev[:9]
	}
	suffix := "+" + rev
	if dirty {
		suffix += "-dirty"
	}
	return Version + suffix
}
