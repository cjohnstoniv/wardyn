// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package movefs

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/movepack"
)

// entry is one entry of a hand-built stream; body is a file's content.
type entry struct {
	path string
	typ  movepack.Type
	mode int64
	body string
	link string
}

func streamOf(t *testing.T, set movepack.Set, es ...entry) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	w, err := movepack.NewWriter(&buf, set)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range es {
		h := movepack.Header{Path: e.path, Type: e.typ, Mode: e.mode, Link: e.link}
		if e.typ == movepack.TypeFile {
			h.Size = int64(len(e.body))
		}
		if err := w.Add(h, strings.NewReader(e.body)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return &buf
}

var workSet = movepack.Set{Roots: []string{"/work"}}

// extractInto makes base/work, the mount root, and extracts the stream there.
func extractInto(t *testing.T, base string, src *bytes.Buffer, set movepack.Set) (movepack.Manifest, error) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(base, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	return Extract(src, ExtractOptions{Set: set, MaxBytes: 1 << 20, Mounts: []string{"/work"}, Base: base})
}

// A symlink planted as a component inside the mount root is never written through: the stream's directory
// at that path is refused, and the directory the link points at stays empty.
func TestMoveFS_ExtractRefusesPlantedSymlink(t *testing.T) {
	base := t.TempDir()
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(base, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../outside", filepath.Join(base, "work/x")); err != nil {
		t.Fatal(err)
	}
	src := streamOf(t, workSet,
		entry{path: "work", typ: movepack.TypeDir, mode: 0o755},
		entry{path: "work/x", typ: movepack.TypeDir, mode: 0o755},
		entry{path: "work/x/f", typ: movepack.TypeFile, mode: 0o644, body: "pwned"},
	)
	if _, err := extractInto(t, base, src, workSet); !errors.Is(err, movepack.ErrArchiveRefused) {
		t.Fatalf("Extract through a planted link = %v, want ErrArchiveRefused", err)
	}
	if ents, _ := os.ReadDir(outside); len(ents) != 0 {
		t.Fatalf("the link's target holds %d entries, want none", len(ents))
	}
}

func TestMoveFS_ExtractExclusive(t *testing.T) {
	src := func() *bytes.Buffer {
		return streamOf(t, workSet,
			entry{path: "work", typ: movepack.TypeDir, mode: 0o755},
			entry{path: "work/d", typ: movepack.TypeDir, mode: 0o755},
			entry{path: "work/d/f", typ: movepack.TypeFile, mode: 0o644, body: "new"},
		)
	}
	base := t.TempDir()
	mustWrite(t, filepath.Join(base, "work/d/f"), "old", 0o644)
	if _, err := extractInto(t, base, src(), workSet); !errors.Is(err, movepack.ErrArchiveRefused) {
		t.Fatalf("Extract over an existing file = %v, want ErrArchiveRefused", err)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "work/d/f")); string(b) != "old" {
		t.Fatalf("the existing file reads %q, want it untouched", b)
	}
	base = t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, "work/d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := extractInto(t, base, src(), workSet); err != nil {
		t.Fatalf("Extract into an existing directory = %v, want nil", err)
	}
	if b, _ := os.ReadFile(filepath.Join(base, "work/d/f")); string(b) != "new" {
		t.Fatalf("the file reads %q, want %q", b, "new")
	}
}

// An entry under no mount root is refused, and nothing is written.
func TestMoveFS_ExtractOnlyBeneathRoots(t *testing.T) {
	base := t.TempDir()
	set := movepack.Set{Roots: []string{"/home"}}
	if err := os.MkdirAll(filepath.Join(base, "home/work"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := streamOf(t, set,
		entry{path: "home", typ: movepack.TypeDir, mode: 0o755},
		entry{path: "home/f", typ: movepack.TypeFile, mode: 0o644, body: "x"},
	)
	_, err := Extract(src, ExtractOptions{Set: set, MaxBytes: 1 << 20, Mounts: []string{"/home/work"}, Base: base})
	if !errors.Is(err, movepack.ErrArchiveRefused) {
		t.Fatalf("Extract of entries under no mount root = %v, want ErrArchiveRefused", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "home/f")); !os.IsNotExist(err) {
		t.Fatalf("home/f was written (%v)", err)
	}
}

// A stream whose manifest does not match makes no symlink, though its files came first.
func TestMoveFS_ExtractSymlinksAfterManifest(t *testing.T) {
	src := streamOf(t, workSet,
		entry{path: "work", typ: movepack.TypeDir, mode: 0o755},
		entry{path: "work/f", typ: movepack.TypeFile, mode: 0o644, body: "x"},
		entry{path: "work/l", typ: movepack.TypeSymlink, mode: 0o777, link: "f"},
	)
	b := src.Bytes()
	i := bytes.LastIndex(b, []byte(`"digest":"`))
	if i < 0 {
		t.Fatal("no digest in the manifest")
	}
	j := i + len(`"digest":"`)
	if b[j] == '0' {
		b[j] = '1'
	} else {
		b[j] = '0'
	}
	base := t.TempDir()
	if _, err := extractInto(t, base, bytes.NewBuffer(b), workSet); !errors.Is(err, movepack.ErrDigestMismatch) {
		t.Fatalf("Extract with a forged manifest = %v, want ErrDigestMismatch", err)
	}
	if _, err := os.Lstat(filepath.Join(base, "work/l")); !os.IsNotExist(err) {
		t.Fatalf("the symlink was made before the manifest matched (%v)", err)
	}
}

// A directory without write access is written into first and given its mode last.
func TestMoveFS_ExtractReadOnlyDirModes(t *testing.T) {
	src := streamOf(t, workSet,
		entry{path: "work", typ: movepack.TypeDir, mode: 0o755},
		entry{path: "work/ro", typ: movepack.TypeDir, mode: 0o555},
		entry{path: "work/ro/f", typ: movepack.TypeFile, mode: 0o444, body: "x"},
		entry{path: "work/ro/sub", typ: movepack.TypeDir, mode: 0o500},
		entry{path: "work/ro/sub/g", typ: movepack.TypeFile, mode: 0o600, body: "y"},
	)
	base := t.TempDir()
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(base, "work/ro/sub"), 0o700)
		_ = os.Chmod(filepath.Join(base, "work/ro"), 0o700)
	})
	if _, err := extractInto(t, base, src, workSet); err != nil {
		t.Fatalf("Extract: %v", err)
	}
	for p, want := range map[string]os.FileMode{"work/ro": 0o555, "work/ro/sub": 0o500, "work/ro/f": 0o444, "work/ro/sub/g": 0o600, "work": 0o755} {
		fi, err := os.Lstat(filepath.Join(base, p))
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode = %o, want %o", p, got, want)
		}
	}
}
