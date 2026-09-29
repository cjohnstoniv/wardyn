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
const Version = "0.8.0"

// releaseBuild is stamped "true" via -ldflags -X, only by release.yml's
// binaries job and Dockerfile.wardynd's RELEASE_BUILD arg (never by
// publish-image.yml's per-commit build). Every other build leaves it empty,
// so String() falls back to the commit suffix; this is the sole mechanism
// that detects a release build.
var releaseBuild string

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
	info, ok := readBuildInfo()
	if !ok {
		return Version
	}
	var rev string
	var dirty bool
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
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
