// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package version holds Wardyn's ONE shipped version string. It lives here
// rather than in a cmd/* main package so the CLI (`wardyn --version`), the
// daemon's startup log and /healthz all report the same build — a corp operator
// diagnosing a CLI/server skew after a rolling upgrade has to be able to ask
// "which build is my control plane running?".
package version

import "runtime/debug"

// Version is the shipped release. cmd/wardyn/version_test.go pins it to the
// newest CHANGELOG.md section and to the other shipped version strings
// (deploy/helm/wardyn/Chart.yaml, ui/package.json) — bump them together.
const Version = "0.7.13"

// releaseBuild is stamped "true" by -ldflags -X, ONLY by the two places
// Wardyn's own tooling builds a release artifact: release.yml's `binaries`
// job and Dockerfile.wardynd's RELEASE_BUILD build-arg (passed by release.yml's
// `images` job, never by publish-image.yml's per-commit main build). Every
// other build — `go test`, `make ci`, a plain `go build`, publish-image.yml's
// image — leaves this empty, so String() falls back to the commit suffix
// below. Neither RELEASING.md nor the Makefile had any ldflags/tag mechanism
// to detect a release build before this; this var IS that mechanism now.
var releaseBuild string

// readBuildInfo is a seam so a test can stand in for the running binary's own
// module info, which debug.ReadBuildInfo reads and nothing else can fake.
var readBuildInfo = debug.ReadBuildInfo

// String reports the build an operator is actually running. A release build
// reports the bare Version (byte-exact, so the release-cut check in
// cmd/wardyn/version_test.go stays true). Any other build appends the commit
// it was built from, and "-dirty" if the working tree had uncommitted changes
// at build time, e.g. "0.7.12+80852d67f" or "0.7.12+80852d67f-dirty" — so a
// pinned main build between releases is identifiable in `wardyn --version`,
// the daemon's startup log and /healthz, which is exactly the question the
// package comment above says it must answer.
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
