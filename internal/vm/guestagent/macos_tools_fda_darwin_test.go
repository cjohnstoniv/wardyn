// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package guestagent

import "testing"

// The real Full Disk Access probe, on a real macOS kernel: it opens the protected TCC database and answers
// one of its three values. Which one depends on the runner's grant, so the test pins only that the probe
// runs and says nothing else.
func TestDarwinFullDiskAccessProbeAnswers(t *testing.T) {
	switch got := (&Tools{}).fullDiskAccess(); got {
	case "allowed", "refused", "":
	default:
		t.Fatalf("fullDiskAccess() = %q, want allowed, refused or empty", got)
	}
}
