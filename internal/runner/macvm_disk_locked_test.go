// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runner

import "testing"

// The org knows a held disk drive's refusal by its sentence, and nothing else looks like it.
func TestIsMacVMDiskLockedKnowsItsSentence(t *testing.T) {
	if !IsMacVMDiskLocked(MacVMDiskLocked("Notes")) {
		t.Fatal("the held-disk sentence is not recognised")
	}
	if IsMacVMDiskLocked("docker: the macOS VM ledger: busy") || IsMacVMDiskLocked(MacVMNoFolderShare) {
		t.Fatal("another error is taken for the held-disk sentence")
	}
}
