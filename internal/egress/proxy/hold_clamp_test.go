// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestConfigureHoldClampsHostileLimits pins the second half of the policy-spec bound. The
// control plane's validatePolicySpec bounds both knobs at every ingest point,
// but it only ever runs at AUTHORING time: a policy stored before the bound
// existed is read straight out of the store, frozen onto the run and handed to
// this sidecar without passing validation again. configureHold is the last
// door before max_holds becomes a channel capacity and hold_seconds becomes
// how long a goroutine sits polling the control plane, so it clamps too.
func TestConfigureHoldClampsHostileLimits(t *testing.T) {
	ap := newApprovalClient("http://127.0.0.1:1", nil, uuid.New(), nil)
	ap.configureHold(types.FirstUseWaitForReview, 2592000*time.Second, 1000000)

	if got := cap(ap.holdSem); got != maxHoldsCeiling {
		t.Errorf("holdSem cap = %d, want clamped to %d", got, maxHoldsCeiling)
	}
	if got := ap.holdTimeout; got != maxHoldTimeout {
		t.Errorf("holdTimeout = %v, want clamped to %v", got, maxHoldTimeout)
	}

	// Negative control: a value production actually passes is untouched.
	ok := newApprovalClient("http://127.0.0.1:1", nil, uuid.New(), nil)
	ok.configureHold(types.FirstUseWaitForReview, 90*time.Second, 4)
	if got := cap(ok.holdSem); got != 4 {
		t.Errorf("holdSem cap = %d, want 4 (in-bounds value must pass through)", got)
	}
	if got := ok.holdTimeout; got != 90*time.Second {
		t.Errorf("holdTimeout = %v, want 90s (in-bounds value must pass through)", got)
	}
}
