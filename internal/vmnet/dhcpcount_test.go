// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"net"
	"testing"
	"time"
)

// TestRelayCountsTheGuestsDHCPRequests: every DHCP request the guest sends
// counts, which is how prepare's stage 1 sees a guest ask for its lease again.
func TestRelayCountsTheGuestsDHCPRequests(t *testing.T) {
	rl, err := New(Config{Prep: true})
	if err != nil {
		t.Fatal(err)
	}
	a, b := net.Pipe()
	go func() { _ = rl.Serve(a) }()
	t.Cleanup(func() { _ = b.Close(); rl.Close() })
	if n := rl.DHCPRequests(); n != 0 {
		t.Fatalf("a new relay counts %d requests", n)
	}
	for i := uint32(1); i <= 2; i++ {
		if err := writeFrame(b, dhcpRequestFrame(dhcpDiscover, i)); err != nil {
			t.Fatal(err)
		}
		if _, err := readReply(b, 3*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if n := rl.DHCPRequests(); n != 2 {
		t.Fatalf("DHCPRequests = %d; want 2", n)
	}
}
