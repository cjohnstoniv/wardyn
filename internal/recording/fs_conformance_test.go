// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package recording_test

import (
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/recording"
	"github.com/cjohnstoniv/wardyn/internal/recording/recordingtest"
)

// The blessed default (fs) must pass the shared store conformance suite — the
// same suite a future object-storage backend will have to pass.
func TestFSStore_Conformance(t *testing.T) {
	recordingtest.RunConformance(t, func(t *testing.T) recording.Store {
		s, err := recording.NewFSStore(t.TempDir())
		if err != nil {
			t.Fatalf("NewFSStore: %v", err)
		}
		return s
	})
}
