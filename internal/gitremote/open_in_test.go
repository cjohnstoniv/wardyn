// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package gitremote

import (
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

// swapForLink renames the folder at and puts a link to target in its place. A target inside the
// tree is given relative: os.Root follows such a link, and refuses an absolute one.
func swapForLink(t *testing.T, at, target string) {
	t.Helper()
	if err := os.Rename(at, at+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
}

// Detection over an open folder reads that folder: its path later pointed at another tree, a
// folder inside it swapped for a link (even to another folder inside), or a file that is a link, is
// not read through.
func TestDetectGitHubReposIn_ReadsTheOpenedFolderThroughNoLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	root, outside := ResolveRoot(t.TempDir()), ResolveRoot(t.TempDir())
	writeRepo(t, filepath.Join(root, "repo"), "https://github.com/acme/inside.git")
	writeRepo(t, filepath.Join(root, "other"), "https://github.com/acme/other.git")
	writeRepo(t, filepath.Join(outside, "repo"), "https://github.com/acme/outside.git")
	open := func(p string) *os.Root {
		dir, err := os.OpenRoot(p)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = dir.Close() })
		return dir
	}

	t.Run("the folder's own path pointed elsewhere", func(t *testing.T) {
		dir := open(root)
		swapForLink(t, root, outside)
		t.Cleanup(func() { _ = os.Remove(root); _ = os.Rename(root+".old", root) })
		if gh, _ := DetectGitHubReposIn(dir); !reflect.DeepEqual(gh, []string{"acme/inside", "acme/other"}) {
			t.Errorf("github = %v, want only the opened folder's repos", gh)
		}
	})
	t.Run("a folder inside swapped for a link to another folder inside", func(t *testing.T) {
		dir := open(root)
		swapForLink(t, filepath.Join(root, "repo"), "other")
		if got := readCapped(dir, "repo/.git/config"); got != nil {
			t.Errorf("read %q through a folder swapped for a link, want nothing", got)
		}
	})
	t.Run("a file that is a link to a file inside", func(t *testing.T) {
		dir := open(root)
		if err := os.Symlink("other/.git/config", filepath.Join(root, "link-config")); err != nil {
			t.Skipf("symlink unsupported: %v", err)
		}
		if f, err := OpenRegularIn(dir, "link-config"); err == nil {
			_ = f.Close()
			t.Error("OpenRegularIn opened a file that is a link")
		}
	})
}
