// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ipguard

import "testing"

// TestTablesMatchPreExtractionLists pins the shared table to the exact CIDR
// set both consumers carried inline before the extraction — any drift here is
// a security-behavior change, not a refactor.
func TestTablesMatchPreExtractionLists(t *testing.T) {
	wantV4 := []string{
		"10.0.0.0/8",
		"172.16.0.0/12",
		"192.168.0.0/16",
		"100.64.0.0/10",
		"0.0.0.0/8",
		"192.0.0.0/24",
		"198.18.0.0/15",
		"255.255.255.255/32",
	}
	if len(PrivateReservedV4) != len(wantV4) {
		t.Fatalf("PrivateReservedV4 has %d entries, want %d", len(PrivateReservedV4), len(wantV4))
	}
	for i, w := range wantV4 {
		if got := PrivateReservedV4[i].String(); got != w {
			t.Errorf("PrivateReservedV4[%d] = %s, want %s", i, got, w)
		}
	}
	if len(UniqueLocalV6) != 1 || UniqueLocalV6[0].String() != "fc00::/7" {
		t.Errorf("UniqueLocalV6 = %v, want exactly [fc00::/7]", UniqueLocalV6)
	}
}
