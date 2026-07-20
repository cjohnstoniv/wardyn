// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"os"
	"regexp"
	"testing"
)

// releaseHeading matches a released CHANGELOG section heading — "## [0.4.0] — …".
// The unreleased placeholder ("## [Unreleased]") does not match, so it may sit
// above the newest release without confusing this.
var releaseHeading = regexp.MustCompile(`(?m)^## \[(\d+\.\d+\.\d+)\]`)

// TestVersionMatchesChangelog pins `wardyn --version` to the newest released
// CHANGELOG section. 0.3.1 shipped with version = "0.3.0" because nothing
// checked; this is that check.
func TestVersionMatchesChangelog(t *testing.T) {
	raw, err := os.ReadFile("../../CHANGELOG.md")
	if err != nil {
		t.Fatalf("read CHANGELOG.md: %v", err)
	}
	m := releaseHeading.FindSubmatch(raw)
	if m == nil {
		t.Fatal("no `## [x.y.z]` release heading found in CHANGELOG.md")
	}
	if got, want := version, string(m[1]); got != want {
		t.Fatalf("version = %q but the newest CHANGELOG section is [%s] — bump cmd/wardyn/main.go or add the section", got, want)
	}
}

// TestShippedVersionStringsAgree extends the same guarantee to the version
// strings no compiler and no other test touches. cmd/wardyn/main.go is now
// pinned above, but the Helm chart and the UI package manifest drift silently:
// values.yaml's comment claimed AppVersion was "0.0.1" while Chart.yaml said
// 0.3.1, and RELEASING.md's steps never mention bumping any of them.
func TestShippedVersionStringsAgree(t *testing.T) {
	for _, tc := range []struct {
		file, pattern string
	}{
		{"../../deploy/helm/wardyn/Chart.yaml", `(?m)^version:\s*(\S+)`},
		{"../../deploy/helm/wardyn/Chart.yaml", `(?m)^appVersion:\s*"?([^"\s]+)"?`},
		{"../../ui/package.json", `(?m)^\s*"version":\s*"([^"]+)"`},
	} {
		raw, err := os.ReadFile(tc.file)
		if err != nil {
			t.Fatalf("read %s: %v", tc.file, err)
		}
		m := regexp.MustCompile(tc.pattern).FindSubmatch(raw)
		if m == nil {
			t.Fatalf("%s: no version matching %s", tc.file, tc.pattern)
		}
		if got := string(m[1]); got != version {
			t.Errorf("%s has %q, want %q (keep every shipped version string in step with cmd/wardyn/main.go)", tc.file, got, version)
		}
	}
}
