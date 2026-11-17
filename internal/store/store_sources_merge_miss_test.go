// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package store

import (
	"errors"
	"testing"
)

// A guarded write that matched no row reports why from the read that follows: the guard (held), a
// missing row, or the read's own fault (never dressed up as a missing row).
func TestMissReadErr(t *testing.T) {
	held := errors.New("guard refused")
	if got := missReadErr(nil, held, "row"); !errors.Is(got, held) {
		t.Errorf("nil read error: got %v, want the held error", got)
	}
	if got := missReadErr(ErrNotFound, held, "row"); !errors.Is(got, ErrNotFound) || errors.Is(got, held) {
		t.Errorf("ErrNotFound read error: got %v, want ErrNotFound only", got)
	}
	fault := errors.New("connection reset")
	got := missReadErr(fault, held, "row")
	if !errors.Is(got, fault) {
		t.Errorf("other read error: got %v, want it to wrap the read's error", got)
	}
	if errors.Is(got, ErrNotFound) || errors.Is(got, held) {
		t.Errorf("other read error: got %v, must be neither ErrNotFound nor the held error", got)
	}
}
