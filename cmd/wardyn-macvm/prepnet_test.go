// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// wiredPrepNet is the prep network prepareDeps wires.
func wiredPrepNet(t *testing.T) macvm.PrepNetwork {
	t.Helper()
	var o macvm.PrepareOptions
	_ = prepareDeps(&o) // refused where this build cannot provision
	if o.PrepNet == nil {
		t.Fatal("prepareDeps wired no prep network")
	}
	nw, err := o.PrepNet()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = nw.Close() })
	return nw
}

// dhcpDiscover is a guest's DHCPDISCOVER as one Ethernet frame.
func dhcpDiscover() []byte {
	bootp := make([]byte, 240, 244)
	bootp[0], bootp[1], bootp[2] = 1, 1, 6       // BOOTREQUEST, Ethernet, hlen 6
	copy(bootp[28:], "\x52\x54\x00\x12\x34\x56") // chaddr
	copy(bootp[236:], []byte{99, 130, 83, 99})   // magic cookie
	bootp = append(bootp, 53, 1, 1, 255)         // DHCPDISCOVER, end
	total := header.EthernetMinimumSize + header.IPv4MinimumSize + header.UDPMinimumSize + len(bootp)
	fr := make([]byte, total)
	header.Ethernet(fr).Encode(&header.EthernetFields{SrcAddr: "\x52\x54\x00\x12\x34\x56", DstAddr: "\xff\xff\xff\xff\xff\xff", Type: header.IPv4ProtocolNumber})
	ip := header.IPv4(fr[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(total - header.EthernetMinimumSize), TTL: 64, Protocol: uint8(header.UDPProtocolNumber),
		SrcAddr: tcpip.AddrFrom4([4]byte{}), DstAddr: tcpip.AddrFrom4([4]byte{255, 255, 255, 255}),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	u := header.UDP(ip.Payload())
	u.Encode(&header.UDPFields{SrcPort: 68, DstPort: 67, Length: uint16(header.UDPMinimumSize + len(bootp))})
	copy(u.Payload(), bootp)
	return fr
}

// TestPrepNetAnswersDHCPAndCountsLeases: a DHCP request on prepare's datagram NIC pair gets
// a reply datagram and is counted.
func TestPrepNetAnswersDHCPAndCountsLeases(t *testing.T) {
	nw := wiredPrepNet(t)
	guest, host, err := macvm.NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	defer host.Close()
	go func() { _ = nw.Serve(host) }()
	sc, err := guest.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	var ioErr error
	_ = sc.Control(func(fd uintptr) {
		tv := unix.NsecToTimeval((5 * time.Second).Nanoseconds())
		if ioErr = unix.SetsockoptTimeval(int(fd), unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); ioErr != nil {
			return
		}
		if _, ioErr = unix.Write(int(fd), dhcpDiscover()); ioErr != nil {
			return
		}
		buf := make([]byte, 1<<16)
		n, ioErr = unix.Read(int(fd), buf)
		if ioErr == nil && (n < header.EthernetMinimumSize || header.Ethernet(buf[:n]).Type() != header.IPv4ProtocolNumber) {
			t.Errorf("reply of %d bytes is not an IPv4 frame", n)
		}
	})
	if ioErr != nil {
		t.Fatalf("no DHCP reply datagram: %v", ioErr)
	}
	if got := nw.DHCPRequests(); got != 1 {
		t.Fatalf("DHCPRequests = %d; want 1", got)
	}
}

// TestPrepNetDialGuestPortRange: a port outside 1-65535 is refused, never
// narrowed into another port.
func TestPrepNetDialGuestPortRange(t *testing.T) {
	nw := wiredPrepNet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for _, port := range []int{0, 70000} {
		if _, err := nw.DialGuest(ctx, port); err == nil || !strings.Contains(err.Error(), "is not 1-65535") {
			t.Fatalf("DialGuest(%d): %v", port, err)
		}
	}
}
