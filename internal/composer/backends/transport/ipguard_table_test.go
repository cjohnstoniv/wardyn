// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package transport

import (
	"net"
	"testing"
)

// TestBlockedRangesMatchPreExtractionLists pins IsBlocked to the exact CIDR set
// this file carried as an inline table before it moved onto internal/ipguard —
// one representative address per range, so a literal that is no longer covered
// fails HERE. Loopback and link-local stay predicate-handled (see TestIsBlocked)
// so allowPrivate semantics are unchanged.
func TestBlockedRangesMatchPreExtractionLists(t *testing.T) {
	blocked := []string{
		"10.0.0.1", "172.16.5.4", "192.168.1.1", // RFC1918
		"100.64.0.1",      // CGNAT
		"0.1.2.3",         // 0.0.0.0/8
		"192.0.0.1",       // IETF protocol assignments
		"198.19.0.1",      // benchmarking
		"255.255.255.255", // limited broadcast
		"fc00::1",         // IPv6 ULA
	}
	for _, s := range blocked {
		if ok, _ := IsBlocked(net.ParseIP(s), false); !ok {
			t.Errorf("IsBlocked(%s) = false, want blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		if ok, why := IsBlocked(net.ParseIP(s), false); ok {
			t.Errorf("IsBlocked(%s) = true (%s), want allowed", s, why)
		}
	}
}
