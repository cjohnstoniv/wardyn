// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package foldersync

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// The numbers this volume gives files, logged for the record, one "file ids:" line per case: a copy's proofs
// (copyAt) rest on them where the volume has no hard links. It fails only when a file operation fails.
func TestCopyFileIDs_HowThisVolumeNumbersFiles(t *testing.T) {
	dir := t.TempDir()
	id := func(fi os.FileInfo) string {
		st := fi.Sys().(*syscall.Stat_t)
		return fmt.Sprintf("dev=%d ino=%d nlink=%d size=%d", st.Dev, st.Ino, st.Nlink, fi.Size())
	}
	create := func(name string) *os.File {
		f, err := os.OpenFile(filepath.Join(dir, name), os.O_RDWR|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = f.Close() })
		return f
	}
	fstat := func(f *os.File) os.FileInfo {
		fi, err := f.Stat()
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	lstat := func(name string) os.FileInfo {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		return fi
	}
	a, b := create("a"), create("b")
	t.Logf("file ids: two empty files: a %s, b %s, same=%v", id(fstat(a)), id(fstat(b)), os.SameFile(fstat(a), fstat(b)))
	before := fstat(a)
	if _, err := a.WriteString("bytes"); err != nil {
		t.Fatal(err)
	}
	if err := a.Sync(); err != nil {
		t.Fatal(err)
	}
	t.Logf("file ids: first write: handle before %s, after %s, name after %s, handle kept=%v, name is handle=%v",
		id(before), id(fstat(a)), id(lstat("a")), os.SameFile(before, fstat(a)), os.SameFile(fstat(a), lstat("a")))
	c := create("c")
	if _, err := c.WriteString("other"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(dir, "c"), filepath.Join(dir, "a")); err != nil {
		t.Fatal(err)
	}
	t.Logf("file ids: rename over an open file: open handle %s, name now %s, same=%v", id(fstat(a)), id(lstat("a")), os.SameFile(fstat(a), lstat("a")))
	if err := os.Remove(filepath.Join(dir, "b")); err != nil {
		t.Fatal(err)
	}
	d := create("d")
	t.Logf("file ids: new empty file while an unlinked empty one is open: unlinked %s, new %s, same=%v", id(fstat(b)), id(fstat(d)), os.SameFile(fstat(b), fstat(d)))
}
