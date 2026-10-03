// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import "testing"

// Only a reason in runner.CapacityBlockerReasons, on any component's detail, is a capacity
// blocker; a pending proxy and the empty detail are not.
func TestCapacityBlockerReason(t *testing.T) {
	for detail, want := range map[string]string{
		"pod: Unschedulable: 0/3 nodes are available: Insufficient cpu.": "Unschedulable",
		"proxy: Pending: waiting":  "",
		"agent: ContainerCreating": "",
		"":                         "",
	} {
		if got := capacityBlockerReason(detail); got != want {
			t.Errorf("capacityBlockerReason(%q) = %q, want %q", detail, got, want)
		}
	}
}
