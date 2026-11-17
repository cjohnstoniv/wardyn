// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"testing"
)

// A requirements merge that matched no row reports why from the read that follows: the cap, a missing
// workspace, or the read's own fault (never dressed up as a missing workspace).
func TestMergeMissErr(t *testing.T) {
	if got := mergeMissErr(nil); !errors.Is(got, ErrConflict) {
		t.Errorf("nil read error: got %v, want ErrConflict", got)
	}
	if got := mergeMissErr(ErrNotFound); !errors.Is(got, ErrNotFound) {
		t.Errorf("ErrNotFound read error: got %v, want ErrNotFound", got)
	}
	fault := errors.New("connection reset")
	got := mergeMissErr(fault)
	if !errors.Is(got, fault) {
		t.Errorf("other read error: got %v, want it to wrap the read's error", got)
	}
	if errors.Is(got, ErrNotFound) || errors.Is(got, ErrConflict) {
		t.Errorf("other read error: got %v, must be neither ErrNotFound nor ErrConflict", got)
	}
}
