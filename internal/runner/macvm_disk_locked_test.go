// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

// The org knows a held disk drive's refusal by its sentence, and nothing else looks like it.
func TestIsMacVMDiskLockedKnowsItsSentence(t *testing.T) {
	if !IsMacVMDiskLocked(MacVMDiskLocked("Notes")) {
		t.Fatal("the held-disk sentence is not recognised")
	}
	// A run that just ended still holds its disk while its VM shuts down, so the sentence says so (#3899).
	const want = "Notes is in another macOS VM on this Mac, or is still being released by a run that just ended. " +
		"A disk drive is in one run at a time: try again in a minute, or when that run ends."
	if got := MacVMDiskLocked("Notes"); got != want {
		t.Fatalf("MacVMDiskLocked = %q, want %q", got, want)
	}
	// An older runner's sentence is still shown as written.
	if !IsMacVMDiskLocked("Notes is in another macOS VM on this Mac. A disk drive is in one run at a time: try again when that run ends.") {
		t.Fatal("an older runner's held-disk sentence is not recognised")
	}
	if IsMacVMDiskLocked("docker: the macOS VM ledger: busy") || IsMacVMDiskLocked(MacVMNoFolderShare) {
		t.Fatal("another error is taken for the held-disk sentence")
	}
}
