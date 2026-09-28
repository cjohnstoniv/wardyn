// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package version

import (
	"runtime/debug"
	"testing"
)

// stubBuildInfo installs a fake debug.ReadBuildInfo for the duration of the
// test, the only way to control what String() sees without actually building
// a binary under different -ldflags.
func stubBuildInfo(t *testing.T, revision string, modified bool, ok bool) {
	t.Helper()
	prev := readBuildInfo
	t.Cleanup(func() { readBuildInfo = prev })
	readBuildInfo = func() (*debug.BuildInfo, bool) {
		if !ok {
			return nil, false
		}
		modifiedStr := "false"
		if modified {
			modifiedStr = "true"
		}
		return &debug.BuildInfo{
			Settings: []debug.BuildSetting{
				{Key: "vcs.revision", Value: revision},
				{Key: "vcs.modified", Value: modifiedStr},
			},
		}, true
	}
}

func withReleaseBuild(t *testing.T, value string) {
	t.Helper()
	prev := releaseBuild
	t.Cleanup(func() { releaseBuild = prev })
	releaseBuild = value
}

// TestString_DevBuild covers the untagged-build case #1105 filed: no
// -ldflags -X stamp, so String() must append the commit (and -dirty when the
// tree was modified) rather than silently repeating the last release.
func TestString_DevBuild(t *testing.T) {
	withReleaseBuild(t, "")

	t.Run("clean", func(t *testing.T) {
		stubBuildInfo(t, "80852d67fabcdef0123456789", false, true)
		if got, want := String(), Version+"+80852d67f"; got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	})

	t.Run("dirty", func(t *testing.T) {
		stubBuildInfo(t, "80852d67fabcdef0123456789", true, true)
		if got, want := String(), Version+"+80852d67f-dirty"; got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	})

	t.Run("no build info", func(t *testing.T) {
		stubBuildInfo(t, "", false, false)
		if got, want := String(), Version; got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	})

	t.Run("no vcs revision", func(t *testing.T) {
		// ok but no vcs.* settings at all — e.g. `go build` outside a VCS checkout.
		stubBuildInfo(t, "", false, true)
		if got, want := String(), Version; got != want {
			t.Fatalf("String() = %q, want %q", got, want)
		}
	})
}

// TestString_ReleaseBuild covers the release build: -ldflags -X stamped
// releaseBuild=true must report the bare Version, ignoring build info
// entirely, so the release-cut check stays byte-exact.
func TestString_ReleaseBuild(t *testing.T) {
	withReleaseBuild(t, "true")
	stubBuildInfo(t, "80852d67fabcdef0123456789", true, true)

	if got, want := String(), Version; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}
