// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"testing"
)

// TestBlockedRangesMatchPreExtractionLists pins isBlockedIP to the exact CIDR
// set this file carried as inline tables before they moved onto internal/ipguard
// + the net.IP predicates — one representative address per range, so a removed
// literal that is no longer covered fails HERE.
func TestBlockedRangesMatchPreExtractionLists(t *testing.T) {
	blocked := []string{
		"10.0.0.1", "172.16.5.4", "192.168.1.1", // RFC1918
		"127.0.0.1",                       // 127.0.0.0/8 loopback
		"169.254.169.254",                 // 169.254.0.0/16 link-local + metadata
		"100.64.0.1",                      // CGNAT
		"0.1.2.3",                         // 0.0.0.0/8
		"192.0.0.1",                       // IETF protocol assignments
		"198.19.0.1",                      // benchmarking
		"255.255.255.255",                 // limited broadcast
		"::1", "fc00::1", "fe80::1", "::", // v6 loopback / ULA / link-local / unspecified
		"::ffff:127.0.0.1", // IPv4-mapped loopback must not smuggle through
	}
	for _, s := range blocked {
		if ok, _ := isBlockedIP(net.ParseIP(s)); !ok {
			t.Errorf("isBlockedIP(%s) = false, want blocked", s)
		}
	}
	for _, s := range []string{"8.8.8.8", "93.184.216.34", "2606:4700:4700::1111"} {
		if ok, why := isBlockedIP(net.ParseIP(s)); ok {
			t.Errorf("isBlockedIP(%s) = true (%s), want allowed", s, why)
		}
	}
}
