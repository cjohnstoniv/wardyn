// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerwire

import (
	"errors"
	"fmt"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runner"
)

// TestSnapshotRefusalRoundTrip: each snapshot refusal crosses the wire as its reason and comes back as its own
// sentinel; any other error carries no reason, and a reason this side does not know is still a refusal.
func TestSnapshotRefusalRoundTrip(t *testing.T) {
	for reason, sentinel := range map[string]error{
		"snapshot_unsupported": runner.ErrSnapshotUnsupported,
		"snapshot_not_pending": runner.ErrSnapshotNotPending,
		"snapshot_in_use":      runner.ErrSnapshotInUse,
		"snapshot_busy":        runner.ErrSnapshotBusy,
		"snapshot_no_space":    runner.ErrSnapshotNoSpace,
		"fork_base_changed":    runner.ErrForkBaseChanged,
	} {
		wrapped := fmt.Errorf("docker: %w: detail", sentinel)
		if got := SnapshotRefusal(wrapped); got != reason {
			t.Errorf("SnapshotRefusal(%v) = %q, want %q", wrapped, got, reason)
		}
		if err := (SnapshotReply{Refusal: reason}).Err(); !errors.Is(err, sentinel) {
			t.Errorf("reason %q comes back as %v, want %v", reason, err, sentinel)
		}
	}
	if got := SnapshotRefusal(errors.New("some other failure")); got != "" {
		t.Errorf("another error carries reason %q", got)
	}
	if err := (SnapshotReply{}).Err(); err != nil {
		t.Errorf("a reply with no refusal = %v", err)
	}
	if err := (SnapshotReply{Refusal: "a_reason_from_a_newer_runner"}).Err(); err == nil {
		t.Error("an unknown reason came back as success")
	}
}
