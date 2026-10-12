// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package runnerpool

import (
	"regexp"
	"slices"
	"testing"
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
