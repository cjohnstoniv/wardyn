// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"
	"time"
)

func TestSiteSessions_UnconfirmedBudget(t *testing.T) {
	n := func(v int) *int { return &v }
	var nilBlock *SiteSessions
	for name, tc := range map[string]struct {
		s    *SiteSessions
		want time.Duration
	}{
		"a nil block": {nilBlock, 30 * time.Second},
		"a nil field": {&SiteSessions{}, 30 * time.Second},
		"0":           {&SiteSessions{UnconfirmedBudgetSec: n(0)}, 0},
		"45":          {&SiteSessions{UnconfirmedBudgetSec: n(45)}, 45 * time.Second},
		"120":         {&SiteSessions{UnconfirmedBudgetSec: n(120)}, 120 * time.Second},
	} {
		if got := tc.s.UnconfirmedBudget(); got != tc.want {
			t.Errorf("%s: UnconfirmedBudget() = %v, want %v", name, got, tc.want)
		}
	}
}
