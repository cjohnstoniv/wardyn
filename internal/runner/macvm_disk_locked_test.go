// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

// The org knows a held disk drive's refusal by its sentence, and nothing else looks like it.
func TestIsMacVMDiskLockedKnowsItsSentence(t *testing.T) {
	if !IsMacVMDiskLocked(MacVMDiskLocked("Notes")) {
		t.Fatal("the held-disk sentence is not recognised")
	}
	// A drive open that is still open holds the disk, and so does a run that just ended while its VM shuts
	// down, so the sentence names both (#3899).
	const want = "Notes is in another macOS VM on this Mac. If it is open there with drive open, close it there, then start the run again. " +
		"Otherwise a run that just ended may still be releasing it: a disk drive is in one run at a time, so try again in a minute, or when that run ends."
	if got := MacVMDiskLocked("Notes"); got != want {
		t.Fatalf("MacVMDiskLocked = %q, want %q", got, want)
	}
	// An older runner's sentences are still shown as written.
	if !IsMacVMDiskLocked("Notes is in another macOS VM on this Mac, or is still being released by a run that just ended. A disk drive is in one run at a time: try again in a minute, or when that run ends.") {
		t.Fatal("the previous runner's held-disk sentence is not recognised")
	}
	if !IsMacVMDiskLocked("Notes is in another macOS VM on this Mac. A disk drive is in one run at a time: try again when that run ends.") {
		t.Fatal("an older runner's held-disk sentence is not recognised")
	}
	if IsMacVMDiskLocked("docker: the macOS VM ledger: busy") || IsMacVMDiskLocked(MacVMNoFolderShare) {
		t.Fatal("another error is taken for the held-disk sentence")
	}
}
