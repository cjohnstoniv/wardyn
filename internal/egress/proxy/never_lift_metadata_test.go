// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"net"
	"testing"
)

// TestMetadataEndpointsAreNeverLiftableKind: the AWS and Google IPv6 metadata addresses
// sit inside the ULA range an operator may lift, so the guard must class them
// with the never-liftable kinds or an internal_hosts entry could reach them.
func TestMetadataEndpointsAreNeverLiftableKind(t *testing.T) {
	for _, ip := range []string{"fd00:ec2::254", "fd00:ec2::23", "fd20:ce::254", "100.100.100.200"} {
		if kind, why := isBlockedIP(net.ParseIP(ip)); kind != blockReservedOther {
			t.Errorf("isBlockedIP(%s) = (%v, %q), want blockReservedOther", ip, kind, why)
		}
	}
	if kind, _ := isBlockedIP(net.ParseIP("fd00::1")); kind != blockPrivate {
		t.Errorf("isBlockedIP(fd00::1) = %v, want blockPrivate (still liftable)", kind)
	}
}
