// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// unfreezable is a runner.Freezer for a sandbox that cannot be frozen.
type unfreezable struct{ calls int }

func (u *unfreezable) FreezeSandbox(context.Context, string) error {
	u.calls++
	return runner.ErrFreezeUnsupported
}

func (u *unfreezable) ThawSandbox(context.Context, string) error {
	u.calls++
	return runner.ErrFreezeUnsupported
}

// A settle record for a sandbox that cannot be frozen (a macOS VM paused under an
// older build) has nothing to move: moveSandbox answers nil at once, so the record
// clears instead of being retried every sweep.
func TestMoveSandboxTreatsUnsupportedFreezeAsNothingToMove(t *testing.T) {
	for _, paused := range []bool{true, false} {
		u := &unfreezable{}
		if err := moveSandbox(t.Context(), u, "ref", paused); err != nil {
			t.Fatalf("paused=%v: %v", paused, err)
		}
		if u.calls != 1 {
			t.Fatalf("paused=%v: %d calls, want 1", paused, u.calls)
		}
	}
}
