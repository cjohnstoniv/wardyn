// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import "testing"

// TestBlockedTablesMatchPreExtractionLists pins the shared-table wiring to the
// exact CIDR set this file carried inline before the internal/ipguard
// extraction — the transport's table is the shared core verbatim (loopback and
// link-local stay predicate-handled so allowPrivate semantics are unchanged).
func TestBlockedTablesMatchPreExtractionLists(t *testing.T) {
	wantV4 := map[string]bool{
		"10.0.0.0/8": true, "172.16.0.0/12": true, "192.168.0.0/16": true,
		"100.64.0.0/10": true, "0.0.0.0/8": true, "192.0.0.0/24": true,
		"198.18.0.0/15": true, "255.255.255.255/32": true,
	}
	if len(blockedV4) != len(wantV4) {
		t.Fatalf("blockedV4 has %d entries, want %d", len(blockedV4), len(wantV4))
	}
	for _, n := range blockedV4 {
		if !wantV4[n.String()] {
			t.Errorf("unexpected blockedV4 entry %s", n)
		}
	}
	if len(blockedV6) != 1 || blockedV6[0].String() != "fc00::/7" {
		t.Errorf("blockedV6 = %v, want exactly [fc00::/7]", blockedV6)
	}
}
