// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitremote

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// TestDetect_SymlinkedRootIsWalked pins the gitremote half of the symlink guard. The
// walk's first callback is the ROOT, which filepath.WalkDir reports from an
// LSTAT — so a scan root that is itself a symlink (~/work -> /mnt/d/work) took
// the "never follow a symlink" arm and the walk ended before it looked at
// anything. Detection then returned zero repos, which is not a failure the
// caller can see: it is silently the same answer as a directory with no git
// remotes, and it costs the run its GitHub grant.
func TestDetect_SymlinkedRootIsWalked(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	real := t.TempDir()
	writeRepo(t, real, "https://github.com/acme/web.git")

	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	gh, other := DetectGitHubRepos(link)
	if want := []string{"acme/web"}; !reflect.DeepEqual(gh, want) {
		t.Errorf("github = %v, want %v", gh, want)
	}
	if len(other) != 0 {
		t.Errorf("otherHosts = %v, want none", other)
	}
}
