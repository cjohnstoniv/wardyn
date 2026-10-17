// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"regexp"
	"slices"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tsList is the string literals of one `NAME = [...]` array in runner-pool-refusals.ts, in order.
func tsList(t *testing.T, name string) []string {
	t.Helper()
	block := regexp.MustCompile(`(?s)` + name + ` = \[(.*?)\]`).FindSubmatch(uiFile(t, "runner-pool-refusals.ts"))
	if block == nil {
		t.Fatalf("runner-pool-refusals.ts has no %s", name)
	}
	var out []string
	for _, m := range regexp.MustCompile(`"([a-z_]+)"`).FindAllSubmatch(block[1], -1) {
		out = append(out, string(m[1]))
	}
	return out
}

// The pool list's reason set and the barrier reason set are the console's, in the same order.
func TestAvailabilityAndBarrierReasonsAreTheTypeScriptOnes(t *testing.T) {
	var avail, barrier []string
	for _, r := range AvailabilityReasons() {
		avail = append(avail, string(r))
	}
	for _, r := range BarrierReasons() {
		barrier = append(barrier, string(r))
	}
	if ts := tsList(t, "POOL_AVAILABILITY_REASONS"); !slices.Equal(avail, ts) {
		t.Errorf("Go availability reasons %v, TypeScript %v", avail, ts)
	}
	if ts := tsList(t, "BARRIER_REASON"); !slices.Equal(barrier, ts) {
		t.Errorf("Go barrier reasons %v, TypeScript %v", barrier, ts)
	}
}

// The organisation's default barrier is the list's default when that barrier is available; one that is
// below the floor, not offered or not allowed leaves the strongest available one, chosen by the server.
func TestBarrierOptionsOrgDefault(t *testing.T) {
	offered := []types.ConfinementClass{types.CC1, types.CC2, types.CC3}
	for _, tc := range []struct {
		name string
		f    BarrierFacts
		want types.ConfinementClass
		by   BarrierDefaultSource
	}{
		{"available", BarrierFacts{Known: true, Offered: offered, Floor: types.CC1, Org: types.CC2}, types.CC2, BarrierDefaultOrganisation},
		{"below the floor", BarrierFacts{Known: true, Offered: offered, Floor: types.CC2, Org: types.CC1}, types.CC3, BarrierDefaultServer},
		{"not offered", BarrierFacts{Known: true, Offered: []types.ConfinementClass{types.CC1, types.CC3}, Org: types.CC2}, types.CC3, BarrierDefaultServer},
		{"not allowed by the pool", BarrierFacts{Known: true, Offered: offered, Org: types.CC2, Limits: &types.RunnerPoolLimits{Barriers: []types.ConfinementClass{types.CC1, types.CC3}}}, types.CC3, BarrierDefaultServer},
		{"none set", BarrierFacts{Known: true, Offered: offered}, types.CC3, BarrierDefaultServer},
	} {
		for _, o := range BarrierOptions(tc.f) {
			if o.Default != (o.Class == tc.want) || (o.Default && o.DefaultSource != tc.by) {
				t.Errorf("%s: %s default=%v from %q, want %s from %q", tc.name, o.Class, o.Default, o.DefaultSource, tc.want, tc.by)
			}
		}
	}
}
