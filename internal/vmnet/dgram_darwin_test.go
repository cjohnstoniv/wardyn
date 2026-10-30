// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"errors"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestServeDatagramSurvivesFullGuestBuffer: on macOS a datagram write to a
// peer whose receive buffer is full may fail with ENOBUFS rather than block.
// 2000 replies into a 64 KiB guest buffer that nobody reads, then a drain:
// the serve is still up and a later DHCP request gets its reply, whichever
// way the kernel behaved.
func TestServeDatagramSurvivesFullGuestBuffer(t *testing.T) {
	rl, log := prepRelay(t)
	defer rl.Close()
	guest, host := datagramPair(t)
	if err := unix.SetsockoptInt(guest, unix.SOL_SOCKET, unix.SO_RCVBUF, 64<<10); err != nil {
		t.Fatal(err)
	}
	d, served := serveNIC(t, rl, host, nil)
	buf := make([]byte, maxFrame+1)
	drain := func() {
		for {
			if _, _, err := unix.Recvfrom(guest, buf, unix.MSG_DONTWAIT); err != nil {
				return
			}
		}
	}
	// The guest end reads nothing until a write of its own is refused, so its
	// buffer fills: the relay's replies are dropped, or the relay waits.
	if err := unix.SetNonblock(guest, true); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2000; {
		_, err := unix.Write(guest, dhcpRequestFrame(dhcpDiscover, uint32(0x10000+i)))
		switch {
		case err == nil:
			i++
		case errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.ENOBUFS):
			drain()
			time.Sleep(time.Millisecond)
		default:
			t.Fatalf("request %d: %v", i, err)
		}
	}
	for deadline := time.Now().Add(20 * time.Second); rl.DHCPRequests() < 2000; time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatalf("the relay took %d of 2000 requests", rl.DHCPRequests())
		}
		drain()
	}
	drain()
	if err := unix.SetNonblock(guest, false); err != nil {
		t.Fatal(err)
	}
	if err := dhcpRoundTrip(t, guest, 0x7fff0000); err != nil {
		t.Fatalf("%v (log %s)", err, log.String())
	}
	select {
	case err := <-served:
		t.Fatalf("the serve ended: %v", err)
	default:
	}
	t.Logf("outbound frames dropped on a full guest buffer: %d", d.dropped.Load())
}
