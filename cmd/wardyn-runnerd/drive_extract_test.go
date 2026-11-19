// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package main

import (
	"archive/tar"
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
)

// driveOpenEntry is one entry of a fake guest's tar stream.
type driveOpenEntry struct {
	name     string
	typeflag byte
	mode     int64
	body     string
	link     string
	size     int64 // when not 0, the size the header claims instead of the body's
}

// driveOpenStream is entries as the bytes tar would send.
func driveOpenStream(t *testing.T, entries ...driveOpenEntry) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	oversize := false
	for _, e := range entries {
		oversize = oversize || e.size != 0
		typeflag := driveOpenFlag(e.typeflag)
		h := &tar.Header{Name: e.name, Typeflag: typeflag, Mode: e.mode, Linkname: e.link, Size: int64(len(e.body))}
		if typeflag != tar.TypeReg {
			h.Size = 0
		}
		if e.size != 0 {
			h.Size = e.size
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(e.body)); err != nil && e.size == 0 {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil && !oversize {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func driveOpenFlag(b byte) byte {
	if b == 0 {
		return tar.TypeReg
	}
	return b
}

// driveOpenRoot is an empty folder, as the destination, with a sibling that nothing may write to.
func driveOpenRoot(t *testing.T) (root *os.Root, dir, sibling string) {
	t.Helper()
	top := t.TempDir()
	dir, sibling = filepath.Join(top, "dest"), filepath.Join(top, "sibling")
	for _, d := range []string{dir, sibling} {
		if err := os.Mkdir(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return root, dir, sibling
}

var driveOpenRoomy = driveOpenLimits{entries: 100, bytes: 1 << 20}

// driveOpenMarks is a mark that notes each file it is given.
func driveOpenMarks(names *[]string) func(*os.File) error {
	return func(f *os.File) error { *names = append(*names, filepath.Base(f.Name())); return nil }
}

// A drive's links, devices, pipes and set-id files are skipped and counted, never followed or made; a
// regular file loses its execute bit and is marked before it is closed.
func TestDriveOpenExtractSkipsWhatItMustNotMake(t *testing.T) {
	root, dir, sibling := driveOpenRoot(t)
	stream := driveOpenStream(t,
		driveOpenEntry{name: "./", typeflag: tar.TypeDir, mode: 0o755},
		driveOpenEntry{name: "./notes/", typeflag: tar.TypeDir, mode: 0o755},
		driveOpenEntry{name: "./notes/a.txt", body: "alpha", mode: 0o755},
		driveOpenEntry{name: "./run.sh", body: "#!/bin/sh\n", mode: 0o4755},
		driveOpenEntry{name: "./group.sh", body: "x", mode: 0o2755},
		driveOpenEntry{name: "./out", typeflag: tar.TypeSymlink, link: sibling},
		driveOpenEntry{name: "./out/escaped.txt", body: "no", mode: 0o644},
		driveOpenEntry{name: "./hard", typeflag: tar.TypeLink, link: "notes/a.txt"},
		driveOpenEntry{name: "./tty", typeflag: tar.TypeChar},
		driveOpenEntry{name: "./disk", typeflag: tar.TypeBlock},
		driveOpenEntry{name: "./pipe", typeflag: tar.TypeFifo},
		driveOpenEntry{name: "./plain", body: "beta", mode: 0o644},
	)
	var marked []string
	c, err := driveOpenExtract(bytes.NewReader(stream), root, driveOpenRoomy, driveOpenMarks(&marked))
	if err != nil {
		t.Fatal(err)
	}
	if want := (driveOpenSkips{driveOpenSkipLink: 2, driveOpenSkipDevice: 2, driveOpenSkipPipe: 1, driveOpenSkipSetID: 2}); c.skipped != want {
		t.Fatalf("skipped = %v, want %v", c.skipped, want)
	}
	if got, want := c.skipped.text(), "Skipped 7: 2 links, 2 devices, 1 pipe, 2 set-id files."; got != want {
		t.Fatalf("text = %q, want %q", got, want)
	}
	// "out" was a skipped link, so out/escaped.txt is a real folder and file under the root.
	if _, err := os.Stat(filepath.Join(sibling, "escaped.txt")); !os.IsNotExist(err) {
		t.Fatalf("a link was followed out of the folder: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(dir, "out"))
	if err != nil || !fi.IsDir() {
		t.Fatalf("out = %v, %v; want a real folder", fi, err)
	}
	for _, name := range []string{"notes/a.txt", "plain", "out/escaped.txt"} {
		fi, err := os.Lstat(filepath.Join(dir, name))
		if err != nil || !fi.Mode().IsRegular() || fi.Mode().Perm()&0o111 != 0 {
			t.Fatalf("%s = %v, %v; want a regular file with no execute bit", name, fi, err)
		}
	}
	for _, name := range []string{"run.sh", "group.sh", "hard", "tty", "disk", "pipe"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was made: %v", name, err)
		}
	}
	// Three files, and the folders notes and out: each is marked once.
	if c.files != 3 || c.bytes != int64(len("alpha")+len("no")+len("beta")) || len(marked) != 5 {
		t.Fatalf("files %d bytes %d marked %v", c.files, c.bytes, marked)
	}
}

// A name that is absolute, leaves the folder, or is over the limits ends the copy; nothing lands outside.
func TestDriveOpenExtractRefusesHostileNames(t *testing.T) {
	long := strings.Repeat("a", driveOpenMaxName+1)
	for _, name := range []string{"../escape", "./a/../../escape", "a/../b", "/etc/escape", "./a/..", long, strings.Repeat("d/", driveOpenMaxPath) + "f"} {
		t.Run(name[:min(len(name), 20)], func(t *testing.T) {
			root, dir, sibling := driveOpenRoot(t)
			stream := driveOpenStream(t, driveOpenEntry{name: "./fine", body: "ok", mode: 0o644}, driveOpenEntry{name: name, body: "bad", mode: 0o644})
			if _, err := driveOpenExtract(bytes.NewReader(stream), root, driveOpenRoomy, func(*os.File) error { return nil }); err == nil {
				t.Fatal("a hostile name was accepted")
			}
			for _, d := range []string{filepath.Dir(dir), sibling} {
				if left, _ := filepath.Glob(filepath.Join(d, "*escape*")); len(left) > 0 {
					t.Fatalf("wrote outside the folder: %v", left)
				}
			}
		})
	}
}

// More entries than the limit, a file past the byte budget, a repeated name, a stream that ends inside a
// file, and a mark that fails each end the copy with an error.
func TestDriveOpenExtractLimitsAndDamage(t *testing.T) {
	ok := func(*os.File) error { return nil }
	tests := []struct {
		name   string
		lim    driveOpenLimits
		stream []byte
		mark   func(*os.File) error
	}{
		{"entries", driveOpenLimits{entries: 2, bytes: 1 << 20}, driveOpenStream(t,
			driveOpenEntry{name: "a", body: "1"}, driveOpenEntry{name: "b", typeflag: tar.TypeSymlink, link: "a"}, driveOpenEntry{name: "c", body: "3"}), ok},
		{"bytes", driveOpenLimits{entries: 10, bytes: 8}, driveOpenStream(t,
			driveOpenEntry{name: "a", body: "12345"}, driveOpenEntry{name: "b", body: "6789"}), ok},
		{"bytes by header", driveOpenLimits{entries: 10, bytes: 8}, driveOpenStream(t, driveOpenEntry{name: "a", body: "1", size: 1 << 40}), ok},
		{"repeated", driveOpenRoomy, driveOpenStream(t, driveOpenEntry{name: "a", body: "1"}, driveOpenEntry{name: "./a", body: "2"}), ok},
		{"folder over file", driveOpenRoomy, driveOpenStream(t, driveOpenEntry{name: "a", body: "1"}, driveOpenEntry{name: "a/b", body: "2"}), ok},
		{"mark fails on a folder", driveOpenRoomy, driveOpenStream(t, driveOpenEntry{name: "./d/", typeflag: tar.TypeDir}), func(*os.File) error { return errors.New("no xattr here") }},
		{"mark fails", driveOpenRoomy, driveOpenStream(t, driveOpenEntry{name: "a", body: "1"}), func(*os.File) error { return errors.New("no xattr here") }},
		{"cut short", driveOpenRoomy, driveOpenStream(t, driveOpenEntry{name: "a", body: "12345"})[:512+3], ok},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root, _, _ := driveOpenRoot(t)
			if c, err := driveOpenExtract(bytes.NewReader(tc.stream), root, tc.lim, tc.mark); err == nil {
				t.Fatalf("copied %+v", c)
			}
		})
	}
}

// Write back sends the new and changed files and new folders, keeps a rewritten file's guest mode, and
// reads no link: a link, a pipe, a hard link and a set-id file are skipped and counted, and a link's target
// never reaches the stream.
func TestDriveOpenPackSendsOnlyChangesAndNoLinks(t *testing.T) {
	root, dir, sibling := driveOpenRoot(t)
	secret := filepath.Join(sibling, "secret.txt")
	if err := os.WriteFile(secret, []byte("do not send"), 0o600); err != nil {
		t.Fatal(err)
	}
	stream := driveOpenStream(t,
		driveOpenEntry{name: "./bin/", typeflag: tar.TypeDir, mode: 0o755},
		driveOpenEntry{name: "./bin/tool", body: "v1", mode: 0o755},
		driveOpenEntry{name: "./same.txt", body: "same", mode: 0o644},
		driveOpenEntry{name: "./gone.txt", body: "gone", mode: 0o644},
	)
	c, err := driveOpenExtract(bytes.NewReader(stream), root, driveOpenRoomy, func(*os.File) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(dir, name)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("bin/tool", "v2 edited")
	write("fresh/new.txt", "new")
	for _, err := range []error{
		os.Symlink(secret, filepath.Join(dir, "link-to-file")),
		os.Symlink(sibling, filepath.Join(dir, "link-to-dir")),
		syscall.Mkfifo(filepath.Join(dir, "pipe"), 0o644),
		os.Link(filepath.Join(dir, "same.txt"), filepath.Join(dir, "hard-same")),
		os.Remove(filepath.Join(dir, "gone.txt")),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	write("setid", "x")
	if err := os.Chmod(filepath.Join(dir, "setid"), os.ModeSetuid|0o755); err != nil {
		t.Fatal(err)
	}

	var out, fresh bytes.Buffer
	back, err := driveOpenPack(root, c, &out, &fresh)
	if err != nil {
		t.Fatal(err)
	}
	if back.changed() != 2 || len(back.known) != 1 || len(back.fresh) != 1 || back.newDirs != 1 || len(back.gone) != 1 || back.gone[0] != "gone.txt" {
		t.Fatalf("back = %+v", back)
	}
	// same.txt and hard-same share an inode, so both are links; the other skips are one each.
	if want := (driveOpenSkips{driveOpenSkipLink: 4, driveOpenSkipPipe: 1, driveOpenSkipSetID: 1}); back.skipped != want {
		t.Fatalf("skipped = %v, want %v", back.skipped, want)
	}
	if bytes.Contains(out.Bytes(), []byte("do not send")) {
		t.Fatal("a link's target reached the stream")
	}
	if bytes.Contains(fresh.Bytes(), []byte("v2 edited")) || bytes.Contains(out.Bytes(), []byte("fresh/")) {
		t.Fatal("a changed file and a new one were sent in the same stream")
	}
	read := func(b *bytes.Buffer) (map[string]*tar.Header, map[string]string) {
		sent, bodies := map[string]*tar.Header{}, map[string]string{}
		tr := tar.NewReader(b)
		for {
			h, err := tr.Next()
			if err != nil {
				return sent, bodies
			}
			body, _ := io.ReadAll(tr)
			sent[h.Name], bodies[h.Name] = h, string(body)
		}
	}
	known, knownBodies := read(&out)
	fresh2, freshBodies := read(&fresh)
	if len(known) != 1 || knownBodies["bin/tool"] != "v2 edited" || known["bin/tool"].Mode != 0o755 {
		t.Fatalf("known stream %v %v", known, knownBodies)
	}
	if len(fresh2) != 2 || fresh2["fresh/"] == nil || freshBodies["fresh/new.txt"] != "new" || fresh2["fresh/new.txt"].Mode != 0o644 {
		t.Fatalf("fresh stream %v %v", fresh2, freshBodies)
	}
}

// A name with a NUL in it is refused whole, as every name the tar writer cannot make is.
func TestDriveOpenNameRules(t *testing.T) {
	for raw, want := range map[string]string{"./": "", ".": "", "./a//b/./c/": "a/b/c", "a b/c": "a b/c"} {
		if got, err := driveOpenName(raw); err != nil || got != want {
			t.Errorf("driveOpenName(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, raw := range []string{"", "/", "/a", "a\x00b", "..", "a/../b", "./.."} {
		if got, err := driveOpenName(raw); err == nil {
			t.Errorf("driveOpenName(%q) = %q, want a refusal", raw, got)
		}
	}
}
