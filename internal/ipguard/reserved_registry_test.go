// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package ipguard

import (
	"net"
	"testing"
)

// TestReservedCoversTheIANANonGloballyReachableBlocks (F115, F131) diffs the
// reserved tables against their canonical surface — the IANA special-purpose
// address registries — rather than against a remembered list of RFCs.
//
// The audit that produced this test found five IPv4 blocks and two IPv6 blocks
// whose registry "Globally Reachable" column reads False, which no stdlib
// predicate covers and which the tables did not list. The internal tell that
// this was an omission and not a decision: 255.255.255.255/32 WAS listed, and it
// is a /32 inside the unlisted 240.0.0.0/4 — the table blocked one address of a
// reserved /4 and left the other ~268 million reachable, and 240/4 is used as
// extra private space inside some estates, i.e. exactly the class of internal
// endpoint this guard is the backstop for.
//
// The allowed half is as load-bearing as the blocked half: three registry rows
// in the same 192.x space ARE globally reachable, so blocking them would be a
// wrong answer of the same kind in the other direction.
func TestReservedCoversTheIANANonGloballyReachableBlocks(t *testing.T) {
	for _, tc := range []struct{ ip, why string }{
		{"240.0.0.1", "240.0.0.0/4 reserved / class E (RFC1112)"},
		{"250.1.2.3", "240.0.0.0/4 reserved, mid-range"},
		{"255.255.255.254", "240.0.0.0/4 reserved, adjacent to the /32 that was already listed"},
		{"192.0.2.5", "192.0.2.0/24 TEST-NET-1 (RFC5737)"},
		{"198.51.100.5", "198.51.100.0/24 TEST-NET-2 (RFC5737)"},
		{"203.0.113.5", "203.0.113.0/24 TEST-NET-3 (RFC5737)"},
		{"192.88.99.1", "192.88.99.0/24 deprecated 6to4 relay anycast (RFC7526)"},
		{"::ffff:240.0.0.1", "v4-mapped spelling of a reserved v4 must not slip past"},
		{"fec0::1", "fec0::/10 deprecated site-local (RFC3879), still configured on some enterprise LANs"},
		{"2002:7f00:0001::1", "2002::/16 6to4 (RFC7526) embeds 127.0.0.1 at bits 16..48, which NAT64EmbeddedV4 cannot read"},
		{"2002::1", "2002::/16 is denied wholesale, not only when its embedded v4 is interesting"},
	} {
		ok, why := PrivateReserved(net.ParseIP(tc.ip))
		if !ok {
			t.Errorf("PrivateReserved(%s) = false, want blocked — %s", tc.ip, tc.why)
			continue
		}
		// Never liftable: an operator's internal_hosts exception reaches
		// RFC1918/ULA/CGNAT only, and none of these are that.
		if InLiftable(net.ParseIP(tc.ip)) {
			t.Errorf("InLiftable(%s) = true (%s), want false — only RFC1918/ULA/CGNAT may be lifted", tc.ip, why)
		}
	}

	// The reason string for the limited-broadcast address must not have moved:
	// 255.255.255.255/32 is listed ahead of the 240.0.0.0/4 that now contains it
	// precisely so a denial keeps naming the narrower, more useful range.
	if _, why := PrivateReserved(net.ParseIP("255.255.255.255")); why != "255.255.255.255/32" {
		t.Errorf("PrivateReserved(255.255.255.255) names %q, want 255.255.255.255/32 — "+
			"the /32 must stay listed ahead of 240.0.0.0/4", why)
	}

	// Registry rows that ARE globally reachable, plus ordinary public space.
	for _, tc := range []struct{ ip, why string }{
		{"192.31.196.1", "192.31.196.0/24 AS112-v4 is globally reachable"},
		{"192.52.193.1", "192.52.193.0/24 AMT is globally reachable"},
		{"192.175.48.1", "192.175.48.0/24 direct delegation AS112 is globally reachable"},
		{"8.8.8.8", "ordinary public v4"},
		{"93.184.216.34", "ordinary public v4"},
		{"2606:4700:4700::1111", "ordinary public v6"},
	} {
		if ok, why := PrivateReserved(net.ParseIP(tc.ip)); ok {
			t.Errorf("PrivateReserved(%s) = true (%s), want allowed — %s", tc.ip, why, tc.why)
		}
	}
}
