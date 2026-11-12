// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/sys/unix"
)

// Every file a copy makes carries com.apple.quarantine, read back from the file itself, in the
// flags;time;agent; shape macOS writes, and so is each folder.
func TestDriveOpenExtractMarksEachFileQuarantined(t *testing.T) {
	root, dir, _ := driveOpenRoot(t)
	stream := driveOpenStream(t,
		driveOpenEntry{name: "./d/", typeflag: '5', mode: 0o755},
		driveOpenEntry{name: "./d/a.txt", body: "alpha", mode: 0o644},
		driveOpenEntry{name: "./b.txt", body: "beta", mode: 0o755})
	c, err := driveOpenExtract(bytes.NewReader(stream), root, driveOpenRoomy, driveOpenMark)
	if err != nil || c.files != 2 {
		t.Fatalf("copy %+v, %v", c, err)
	}
	for _, name := range []string{"d/a.txt", "b.txt"} {
		buf := make([]byte, 128)
		n, err := unix.Getxattr(filepath.Join(dir, name), driveOpenQuarantine, buf)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if v := string(buf[:n]); !strings.HasPrefix(v, "0081;") || !strings.HasSuffix(v, ";Wardyn;") {
			t.Fatalf("%s quarantine = %q", name, v)
		}
	}
	buf := make([]byte, 128)
	if n, err := unix.Getxattr(filepath.Join(dir, "d"), driveOpenQuarantine, buf); err != nil || !strings.HasPrefix(string(buf[:n]), "0081;") {
		t.Fatalf("the folder d was not marked: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(dir, "b.txt")); fi.Mode().Perm()&0o111 != 0 {
		t.Fatalf("execute bit kept: %v", fi.Mode())
	}
}
