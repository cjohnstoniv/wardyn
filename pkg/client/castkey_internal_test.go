// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package client

// White-box (package client, not client_test): castKey/castKeySep are
// unexported by design (see their own doc comment — a deliberate copy of
// internal/recording.CastKey/castSep, not an import of it), so proving they
// still agree needs access to both unexported halves and can only live here.

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
)

// TestCastKeyMirrorsRecording: CastKey is inlined into pkg/client (to keep
// sdk.md's "one non-stdlib dependency" claim true — see GetRecording's own
// comment), which makes it a by-hand-synced copy; this proves the two stay in
// sync. A test-only import of internal/recording costs pkg/client's real
// consumers nothing (test files never ship).
func TestCastKeyMirrorsRecording(t *testing.T) {
	for _, tc := range []struct{ runID, suffix string }{
		{"id", ""},
		{"id", "sess"},
	} {
		got := castKey(tc.runID, tc.suffix)
		want := recording.CastKey(tc.runID, tc.suffix)
		if got != want {
			t.Errorf("castKey(%q, %q) = %q, want %q (recording.CastKey) — castKeySep drifted from internal/recording's own castSep", tc.runID, tc.suffix, got, want)
		}
	}
}
