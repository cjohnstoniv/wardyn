// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package workspacescan

import (
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"syscall"
	"testing"
	"time"
)

// TestCollectFacts_SymlinkedRootIsScanned pins B11b-F1: a scan root that is
// ITSELF a symlink (~/work -> /mnt/d/work, macOS /tmp, a WSL drive shortcut)
// scanned as EMPTY and — because nothing was truncated and nothing was
// unrecognized — the empty result graded high confidence with NeedsReview
// false. filepath.WalkDir LSTATS the root, so the callback's ModeSymlink check
// fired on the FIRST call and ended the whole walk, while dispatch went on to
// mount the RESOLVED tree. The scanner's whole output was a silent false green
// about a directory full of code.
func TestCollectFacts_SymlinkedRootIsScanned(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ on windows")
	}
	real := t.TempDir()
	writeFile(t, real, "go.mod", "module x\n\ngo 1.22\n")

	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	got := Scan(link)
	eq(t, "Languages", got.Languages, []string{"Go"})
	if got.Confidence != ConfidenceHigh {
		t.Errorf("Confidence = %q, want %q", got.Confidence, ConfidenceHigh)
	}
}

// TestCollectFacts_FifoDoesNotWedgeTheWalk pins the cross-lane half of
// B11b-F1: every file the walk opens went through a bare os.Open, and
// CollectFacts runs on an HTTP handler goroutine with NO ctx — a FIFO named
// after any file the scan reads (an unrecognized build descriptor, a
// Dockerfile whose content is hashed, a package.json, a compose file the
// content lane reads line by line) blocks open(2) forever waiting for a
// writer, wedging that request permanently. Same defect, same fix, as the
// gitremote reader beside it.
func TestCollectFacts_FifoDoesNotWedgeTheWalk(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("mkfifo is POSIX-only")
	}
	root := t.TempDir()
	writeFile(t, root, "go.mod", "module x\n\ngo 1.22\n")
	for _, name := range []string{
		"build.xml",          // unrecognized build descriptor: readCapped
		"Dockerfile",         // build-input hash: hashFileContent
		"package.json",       // detectPackageJSON
		"docker-compose.yml", // content lane: eachLine
	} {
		if err := syscall.Mkfifo(filepath.Join(root, name), 0o644); err != nil {
			t.Skipf("mkfifo unsupported: %v", err)
		}
	}

	done := make(chan WorkspaceProfile, 1)
	go func() { done <- Scan(root) }()
	select {
	case got := <-done:
		// Marker detection is filename-keyed and opens nothing, so a FIFO
		// NAMED package.json still reads as JavaScript — that is the existing
		// contract and not what this test is about. What matters is that the
		// walk finished and the real manifest beside the FIFOs was seen.
		if !slices.Contains(got.Languages, "Go") {
			t.Errorf("Languages = %v, want Go among them", got.Languages)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Scan blocked on a FIFO: the walk's file reads must never wait for a writer")
	}
}
