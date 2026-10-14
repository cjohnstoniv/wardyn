// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recoveryset

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func write(t *testing.T, path, body string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), perm); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, perm); err != nil {
		t.Fatal(err)
	}
}

func digest(t *testing.T, dir string) Digest {
	t.Helper()
	d, err := TreeDigest(dir)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestTreeDigest_IsIndependentOfCreationOrder(t *testing.T) {
	a, b := t.TempDir(), t.TempDir()
	for _, n := range []string{"a.b", "a/b", "z", "a/c"} {
		write(t, filepath.Join(a, n), n, 0o644)
	}
	for _, n := range []string{"a/c", "z", "a/b", "a.b"} {
		write(t, filepath.Join(b, n), n, 0o644)
	}
	if da, db := digest(t, a), digest(t, b); da != db || da.Entries != 5 {
		t.Errorf("digests %+v and %+v; want equal with 5 entries (a, a.b, a/b, a/c, z)", da, db)
	}
}

func TestTreeDigest_ChangesWithAPermAByteAndAName(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "f"), "hello", 0o644)
	base := digest(t, dir)

	if err := os.Chmod(filepath.Join(dir, "f"), 0o600); err != nil {
		t.Fatal(err)
	}
	if digest(t, dir) == base {
		t.Error("a perm change left the digest unchanged")
	}
	write(t, filepath.Join(dir, "f"), "hellp", 0o644)
	if digest(t, dir) == base {
		t.Error("a one-byte flip left the digest unchanged")
	}
	write(t, filepath.Join(dir, "f"), "hello", 0o644)
	if digest(t, dir) != base {
		t.Fatal("restoring the byte did not restore the digest")
	}
	if err := os.Rename(filepath.Join(dir, "f"), filepath.Join(dir, "g")); err != nil {
		t.Fatal(err)
	}
	if digest(t, dir) == base {
		t.Error("a rename left the digest unchanged")
	}
}

func TestTreeDigest_IgnoresTimes(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "f"), "hello", 0o644)
	base := digest(t, dir)
	old := time.Now().Add(-72 * time.Hour)
	if err := os.Chtimes(filepath.Join(dir, "f"), old, old); err != nil {
		t.Fatal(err)
	}
	if digest(t, dir) != base {
		t.Error("an mtime change moved the digest")
	}
}

func TestTreeDigest_NeverFollowsASymlink(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "secret"), "one", 0o644)
	dir := t.TempDir()
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	base := digest(t, dir)
	write(t, filepath.Join(outside, "secret"), "two", 0o644)
	if digest(t, dir) != base {
		t.Error("the digest follows the link: changing its target's content moved it")
	}
	if err := os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "other"), filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if digest(t, dir) == base {
		t.Error("re-pointing a link left the digest unchanged")
	}
}

func TestTreeDigest_RefusesAFIFONamingIt(t *testing.T) {
	dir := t.TempDir()
	if err := syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644); err != nil {
		t.Skip("no FIFO here:", err)
	}
	_, err := TreeDigest(dir)
	if err == nil || !strings.Contains(err.Error(), "pipe") {
		t.Errorf("err = %v; want a refusal naming the FIFO", err)
	}
}

func TestTreeDigest_ReadsAReadOnlyTree(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "d", "f"), "x", 0o444)
	if err := os.Chmod(filepath.Join(dir, "d"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "d"), 0o755) })
	if d := digest(t, dir); d.Entries != 2 {
		t.Errorf("entries = %d, want 2", d.Entries)
	}
}

// The digest is sha256 over these lines, sorted bytewise by path: "a", "a.b", "a/b" ('.' sorts before '/').
func TestTreeDigest_MatchesTheDocumentedLineFormat(t *testing.T) {
	dir := t.TempDir()
	write(t, filepath.Join(dir, "a.b"), "x", 0o644)
	write(t, filepath.Join(dir, "a", "b"), "y", 0o640)
	if err := os.Chmod(filepath.Join(dir, "a"), 0o750); err != nil {
		t.Fatal(err)
	}
	sum := func(s string) string { h := sha256.Sum256([]byte(s)); return hex.EncodeToString(h[:]) }
	lines := "\"a\"\td\t0750\t0\t\n" +
		"\"a.b\"\tf\t0644\t1\t" + sum("x") + "\n" +
		"\"a/b\"\tf\t0640\t1\t" + sum("y") + "\n"
	want := Digest{Entries: 3, SHA256: sum(lines)}
	if got := digest(t, dir); got != want {
		t.Errorf("digest = %+v; want %+v", got, want)
	}
}

func TestTreeDigest_ChangesWithSetuidSetgidAndSticky(t *testing.T) {
	for name, bit := range map[string]os.FileMode{"setuid": os.ModeSetuid, "setgid": os.ModeSetgid, "sticky": os.ModeSticky} {
		dir := t.TempDir()
		write(t, filepath.Join(dir, "f"), "x", 0o755)
		base := digest(t, dir)
		if err := os.Chmod(filepath.Join(dir, "f"), 0o755|bit); err != nil {
			t.Fatal(err)
		}
		if digest(t, dir) == base {
			t.Errorf("%s left the digest unchanged", name)
		}
	}
}

func TestTreeDigestIn_RefusesAPathThatLeavesTheRoot(t *testing.T) {
	outside := t.TempDir()
	write(t, filepath.Join(outside, "f"), "x", 0o644)
	set := t.TempDir()
	if err := os.Mkdir(filepath.Join(set, "drives"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(set, "drives", "id")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(set)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := TreeDigestIn(root, "drives/id"); err == nil {
		t.Error("a symlink out of the set was read")
	}
}
