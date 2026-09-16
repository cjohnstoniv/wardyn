// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"fmt"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// TestValidatePolicySpec_HoldBounds pins B11b-F2. first_use_hold_seconds and
// max_holds were the last two policy knobs NOTHING bounded: composer.Clamp
// never visits them, validatePolicySpec never read them, and the proxy turns
// max_holds straight into a channel capacity and hold_seconds into how long a
// goroutine sits there polling the control plane once a second. A member's
// inline_policy under a wait_for_review ceiling could therefore author
// {max_holds: 1000000, first_use_hold_seconds: 2592000} and defeat the
// documented 16-hold / 30-second default with a million held goroutines
// polling for thirty days.
//
// validatePolicySpec is the right door because EVERY ingest point crosses it:
// the stored-policy write, the inline run policy, the WARDYN_DEFAULT_POLICY
// file, and the composer/profile clamp.
func TestValidatePolicySpec_HoldBounds(t *testing.T) {
	spec := func(holdSec, maxHolds int) types.RunPolicySpec {
		return types.RunPolicySpec{
			MinConfinementClass: types.CC2,
			FirstUseApproval:    types.FirstUseWaitForReview,
			FirstUseHoldSeconds: holdSec,
			MaxHolds:            maxHolds,
		}
	}

	// Asserted THROUGH the DRAFT constants, never against a literal: an M2
	// rewording of either refusal must not need this file edited to stay true.
	holdsRefusal := func(got int) string { return fmt.Sprintf(maxHoldsRefusal, maxHoldsPerSpec, got) }
	secondsRefusal := func(got int) string {
		return fmt.Sprintf(firstUseHoldSecondsRefusal, maxFirstUseHoldSeconds, got)
	}

	refused := []struct {
		name         string
		spec         types.RunPolicySpec
		wantContains string
	}{
		{"max_holds above the cap", spec(0, maxHoldsPerSpec+1), holdsRefusal(maxHoldsPerSpec + 1)},
		{"max_holds absurd (the member-inline case)", spec(0, 1000000), holdsRefusal(1000000)},
		{"max_holds negative", spec(0, -1), holdsRefusal(-1)},
		{"hold seconds above the cap", spec(maxFirstUseHoldSeconds+1, 0), secondsRefusal(maxFirstUseHoldSeconds + 1)},
		{"hold seconds a month", spec(2592000, 0), secondsRefusal(2592000)},
		{"hold seconds negative", spec(-1, 0), secondsRefusal(-1)},
	}
	for _, tc := range refused {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicySpec(tc.spec)
			if err == nil {
				t.Fatalf("accepted %+v", tc.spec)
			}
			if !strings.Contains(err.Error(), tc.wantContains) {
				t.Errorf("error %q does not name %q", err, tc.wantContains)
			}
		})
	}

	// Negative control: the values production actually uses. 0 means "keep the
	// built-in default" on both knobs and must stay valid, and a real authored
	// value at or under the cap must pass unchanged.
	for _, ok := range []types.RunPolicySpec{
		spec(0, 0),
		spec(30, 16),
		spec(maxFirstUseHoldSeconds, maxHoldsPerSpec),
	} {
		if err := validatePolicySpec(ok); err != nil {
			t.Errorf("rejected a policy inside the bounds %+v: %v", ok, err)
		}
	}
}
