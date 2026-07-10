// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import "testing"

// TestBlockedTablesMatchPreExtractionLists pins the composed tables (shared
// internal/ipguard core + proxy-only entries) to the exact CIDR sets this file
// carried inline before the extraction. Set-equality: table order never
// mattered (every lookup scans the whole table).
func TestBlockedTablesMatchPreExtractionLists(t *testing.T) {
	want := map[string][]string{
		"v4": {
			"10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16",
			"127.0.0.0/8", "169.254.0.0/16", "100.64.0.0/10",
			"0.0.0.0/8", "192.0.0.0/24", "198.18.0.0/15", "255.255.255.255/32",
		},
		"v6":    {"::1/128", "fc00::/7", "fe80::/10", "::/128"},
		"nat64": {"64:ff9b::/96", "64:ff9b:1::/48"},
	}
	got := map[string]map[string]bool{"v4": {}, "v6": {}, "nat64": {}}
	for _, n := range blockedV4 {
		got["v4"][n.String()] = true
	}
	for _, n := range blockedV6 {
		got["v6"][n.String()] = true
	}
	for _, n := range nat64Prefixes {
		got["nat64"][n.String()] = true
	}
	for k, ws := range want {
		if len(got[k]) != len(ws) {
			t.Errorf("%s table has %d entries, want %d (%v)", k, len(got[k]), len(ws), got[k])
		}
		for _, w := range ws {
			if !got[k][w] {
				t.Errorf("%s table missing %s", k, w)
			}
		}
	}
}
