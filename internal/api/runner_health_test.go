// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// A runner's clock is its own: a failure or relay time more than runnerClockSkew ahead of the organisation's is
// dropped, so a report cannot pin itself as the newest.
func TestRunnerHealthDropsATimeFarAhead(t *testing.T) {
	now := time.Now().UTC()
	sum := func(at time.Time) *types.RunnerOfflineSummary {
		return &types.RunnerOfflineSummary{Stage: types.RunnerStageDNS, FirstFailureAt: now.Add(-time.Minute), LastFailureAt: at}
	}
	rel := func(at time.Time) *types.RunnerRelayStatus {
		return &types.RunnerRelayStatus{State: types.RelayUnreachable, Since: at}
	}
	for name, c := range map[string]struct {
		at   time.Time
		keep bool
	}{
		"now":             {now, true},
		"inside the skew": {now.Add(runnerClockSkew), true},
		"past the skew":   {now.Add(runnerClockSkew + time.Second), false},
		"a day ahead":     {now.Add(24 * time.Hour), false},
	} {
		if got := checkedOfflineSummary(sum(c.at), now) != nil; got != c.keep {
			t.Errorf("%s: offline summary kept = %v, want %v", name, got, c.keep)
		}
		if got := checkedRelayStatus(rel(c.at), now) != nil; got != c.keep {
			t.Errorf("%s: relay status kept = %v, want %v", name, got, c.keep)
		}
	}
}
