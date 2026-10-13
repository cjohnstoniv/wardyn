// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package spoolowner

import (
	"os"
	"path/filepath"
	"testing"
)

// Keep hands a file to the directory's owner only when this process is another user, through the open
// file, and never a file with a second link.
func TestKeepHandsOnlyASingleLinkFileToTheDirectoryOwner(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	owner := os.Geteuid()
	var handed []int
	Chown = func(f *os.File, uid, gid int) error { handed = append(handed, uid); return f.Chown(uid, gid) }
	t.Cleanup(func() {
		ProcessUID, Chown = os.Geteuid, func(f *os.File, uid, gid int) error { return f.Chown(uid, gid) }
	})
	open := func(name string) *os.File {
		f, err := root.OpenFile(name, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { f.Close() })
		return f
	}

	if err := Keep(root, open("mine")); err != nil || len(handed) != 0 {
		t.Fatalf("the directory's own owner: %v, handed %v; want nothing done", err, handed)
	}
	ProcessUID = func() int { return owner + 1 }
	if err := Keep(root, open("theirs")); err != nil || len(handed) != 1 || handed[0] != owner {
		t.Fatalf("another user's file: %v, handed %v; want it given to the directory's owner %d", err, handed, owner)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(outside, filepath.Join(dir, "linked")); err != nil {
		t.Skipf("hard links are not available here: %v", err)
	}
	if err := Keep(root, open("linked")); err == nil || len(handed) != 1 {
		t.Fatalf("a file with a second link: %v, handed %v; want it refused and left as it is", err, handed)
	}
}
