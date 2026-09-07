// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"testing"
)

// TestIsBlockedIPCoversTheReservedResidual (F115, F131) is the proxy-side half
// of the ipguard registry pin: the classification, not just the membership.
//
// isBlockedIP composes net.IP's loopback/link-local/multicast/unspecified
// predicates with ipguard.PrivateReserved and a NAT64 re-check, and three
// classes fell through it as blockNone — IPv4 reserved 240.0.0.0/4, the three
// TEST-NETs plus the deprecated 6to4 relay anycast, IPv6 site-local fec0::/10,
// and 6to4 2002::/16 (whose bits 16..48 embed a real IPv4 exactly as NAT64 does,
// which the NAT64-scoped extraction cannot read). VetHost returned Denied=false
// for every one, so a hostname resolving into any of them was dialled.
//
// Every one must land as blockReservedOther: denied, and NEVER liftable — an
// operator's internal_hosts exception reaches RFC1918/ULA/CGNAT only, and the
// lift is offered only for blockPrivate.
func TestIsBlockedIPCoversTheReservedResidual(t *testing.T) {
	for _, ip := range []string{
		"240.0.0.1", "250.1.2.3", "255.255.255.254",
		"192.0.2.5", "198.51.100.5", "203.0.113.5", "192.88.99.1",
		"fec0::1", "2002:7f00:0001::1",
	} {
		kind, why := isBlockedIP(net.ParseIP(ip))
		if kind != blockReservedOther {
			t.Errorf("isBlockedIP(%s) = kind %d (%q), want blockReservedOther — "+
				"the unconditional guard is what docs/OPERATIONS.md and THREAT-MODEL.md promise denies "+
				"'other reserved ranges' regardless of policy", ip, kind, why)
		}
		if g := VetHost(ip, nil); !g.Denied {
			t.Errorf("VetHost(%s).Denied = false, want true", ip)
		}
	}
	// The liftable set is unchanged: an operator's declared internal host still
	// reaches RFC1918/ULA/CGNAT, which this widening must not have swept up.
	for _, ip := range []string{"10.0.0.5", "172.16.0.1", "192.168.1.1", "fd00::1", "100.64.0.1"} {
		if kind, why := isBlockedIP(net.ParseIP(ip)); kind != blockPrivate {
			t.Errorf("isBlockedIP(%s) = kind %d (%q), want blockPrivate (the ONE liftable kind)", ip, kind, why)
		}
	}
	// And ordinary public space still passes.
	for _, ip := range []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		if kind, why := isBlockedIP(net.ParseIP(ip)); kind != blockNone {
			t.Errorf("isBlockedIP(%s) = kind %d (%q), want blockNone", ip, kind, why)
		}
	}
}
