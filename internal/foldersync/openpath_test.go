// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// OpenFromTop opens a path with no link in it, and refuses one with a link at any folder on the
// way, including a folder swapped for a link between its look and its open.
func TestOpenFromTop_RefusesALinkAnywhereOnThePath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(base, "a", "b")
	if err := os.MkdirAll(real, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "a"), filepath.Join(base, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	dir, err := OpenFromTop(real)
	if err != nil {
		t.Fatalf("OpenFromTop(%s) = %v, want the folder", real, err)
	}
	if fi, err := dir.Stat("."); err != nil || filepath.Clean(dir.Name()) != real {
		t.Errorf("handle %q (%v, %v), want %q", dir.Name(), fi, err, real)
	}
	_ = dir.Close()
	if _, err := OpenFromTop(filepath.Join(base, "link", "b")); !errors.Is(err, ErrLinkInPath) {
		t.Errorf("a link on the way: err = %v, want ErrLinkInPath", err)
	}

	// childLooked runs once for each folder from the top, so a's own look is one call past base's
	// folders. The link goes to another folder inside base: os.Root follows such a link.
	if err := os.MkdirAll(filepath.Join(base, "elsewhere", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	calls, atA := 0, strings.Count(filepath.ToSlash(base), "/")+1
	old := childLooked
	childLooked = func() {
		if calls++; calls != atA {
			return
		}
		a := filepath.Join(base, "a")
		if err := os.Rename(a, a+".old"); err != nil {
			t.Error(err)
		}
		if err := os.Symlink("elsewhere", a); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { childLooked = old })
	if d, err := OpenFromTop(real); !errors.Is(err, ErrLinkInPath) {
		if d != nil {
			_ = d.Close()
		}
		t.Errorf("a folder swapped for a link between its look and its open: err = %v, want ErrLinkInPath", err)
	}
	if calls < atA {
		t.Fatalf("the swap never ran (%d looks, a is look %d): the pin did not run its case", calls, atA)
	}
}
