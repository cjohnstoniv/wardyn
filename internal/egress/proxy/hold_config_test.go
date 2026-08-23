// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestConfigureHoldOverridesAndDefaults pins D8: a non-default hold timeout /
// max-holds reaches the hold config, and 0/absent keeps the built-in
// 30s / 16 defaults (back-compat — what production passed before the knobs).
func TestConfigureHoldOverridesAndDefaults(t *testing.T) {
	// Defaults: newApprovalClient seeds 30s / cap 16; configureHold(_,0,0) keeps them.
	ap := newApprovalClient("http://cp", nil, uuid.Nil, nil)
	ap.configureHold(types.FirstUseWaitForReview, 0, 0)
	if ap.holdTimeout != defaultHoldTimeout {
		t.Fatalf("default holdTimeout = %v, want %v", ap.holdTimeout, defaultHoldTimeout)
	}
	if got := cap(ap.holdSem); got != defaultMaxHolds {
		t.Fatalf("default holdSem cap = %d, want %d", got, defaultMaxHolds)
	}

	// Override: a non-default value from the policy reaches the hold config.
	ap.configureHold(types.FirstUseWaitForReview, 90*time.Second, 4)
	if ap.holdTimeout != 90*time.Second {
		t.Fatalf("override holdTimeout = %v, want 90s", ap.holdTimeout)
	}
	if got := cap(ap.holdSem); got != 4 {
		t.Fatalf("override holdSem cap = %d, want 4", got)
	}
}
