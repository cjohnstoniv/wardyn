// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/macvm"
	"golang.org/x/sys/unix"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// wiredPrepNet is the prep network prepareDeps wires, logging to root's
// prep-relay.log as cmdPrepare opens it.
func wiredPrepNet(t *testing.T, root string) macvm.PrepNetwork {
	t.Helper()
	relayLog, closeLog, err := openRelayLog(root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(closeLog)
	var o macvm.PrepareOptions
	_ = prepareDeps(&o, relayLog) // refused where this build cannot provision
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

const testGuestMAC = "\x52\x54\x00\x12\x34\x56"

// udpFrame is one IPv4/UDP Ethernet frame from the guest.
func udpFrame(dstMAC string, src, dst [4]byte, sport, dport uint16, payload []byte) []byte {
	total := header.EthernetMinimumSize + header.IPv4MinimumSize + header.UDPMinimumSize + len(payload)
	fr := make([]byte, total)
	header.Ethernet(fr).Encode(&header.EthernetFields{SrcAddr: testGuestMAC, DstAddr: tcpip.LinkAddress(dstMAC), Type: header.IPv4ProtocolNumber})
	ip := header.IPv4(fr[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(total - header.EthernetMinimumSize), TTL: 64, Protocol: uint8(header.UDPProtocolNumber),
		SrcAddr: tcpip.AddrFrom4(src), DstAddr: tcpip.AddrFrom4(dst),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	u := header.UDP(ip.Payload())
	u.Encode(&header.UDPFields{SrcPort: sport, DstPort: dport, Length: uint16(header.UDPMinimumSize + len(payload))})
	copy(u.Payload(), payload)
	return fr
}

// dhcpDiscover is a guest's DHCPDISCOVER as one Ethernet frame.
func dhcpDiscover() []byte {
	bootp := make([]byte, 240, 244)
	bootp[0], bootp[1], bootp[2] = 1, 1, 6     // BOOTREQUEST, Ethernet, hlen 6
	copy(bootp[28:], testGuestMAC)             // chaddr
	copy(bootp[236:], []byte{99, 130, 83, 99}) // magic cookie
	bootp = append(bootp, 53, 1, 1, 255)       // DHCPDISCOVER, end
	return udpFrame("\xff\xff\xff\xff\xff\xff", [4]byte{}, [4]byte{255, 255, 255, 255}, 68, 67, bootp)
}

// guestFD runs fn on the guest end's descriptor, with a 5 s receive timeout.
func guestFD(t *testing.T, guest *os.File, fn func(fd int)) {
	t.Helper()
	sc, err := guest.SyscallConn()
	if err != nil {
		t.Fatal(err)
	}
	_ = sc.Control(func(fd uintptr) {
		tv := unix.NsecToTimeval((5 * time.Second).Nanoseconds())
		if err := unix.SetsockoptTimeval(int(fd), unix.SOL_SOCKET, unix.SO_RCVTIMEO, &tv); err != nil {
			t.Error(err)
			return
		}
		fn(int(fd))
	})
}

// TestPrepNetAnswersDHCPAndCountsLeases: a DHCP request on prepare's datagram NIC pair gets
// a reply datagram and is counted.
func TestPrepNetAnswersDHCPAndCountsLeases(t *testing.T) {
	nw := wiredPrepNet(t, t.TempDir())
	guest, host, err := macvm.NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	defer host.Close()
	go func() { _ = nw.Serve(host) }()
	var n int
	var ioErr error
	guestFD(t, guest, func(fd int) {
		if _, ioErr = unix.Write(fd, dhcpDiscover()); ioErr != nil {
			return
		}
		buf := make([]byte, 1<<16)
		n, ioErr = unix.Read(fd, buf)
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

// TestPrepRelayLogTakesRefusals: a guest flow the prep relay refuses is in
// root's prep-relay.log, and nothing of the relay reaches the helper's
// stdout or stderr, which runnerd collects into prepare.log and whose last
// line it reads as progress.
func TestPrepRelayLogTakesRefusals(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	restored := false
	restore := func() {
		if !restored {
			os.Stdout, os.Stderr, restored = stdout, stderr, true
		}
	}
	defer restore()

	root := t.TempDir()
	nw := wiredPrepNet(t, root)
	guest, host, err := macvm.NewNetPair()
	if err != nil {
		t.Fatal(err)
	}
	defer guest.Close()
	defer host.Close()
	go func() { _ = nw.Serve(host) }()
	// An NTP query to an Apple host, as a macOS guest sends at first login.
	ntp := udpFrame("\x5a\x94\xef\xe4\x0c\xdd", [4]byte{10, 0, 2, 15}, [4]byte{17, 253, 144, 10}, 50123, 123, make([]byte, 48))
	guestFD(t, guest, func(fd int) {
		if _, err := unix.Write(fd, ntp); err != nil {
			t.Error(err)
		}
	})
	var logged string
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		b, _ := os.ReadFile(macvm.Layout{Root: root}.PrepRelayLog())
		if logged = string(b); strings.Contains(logged, `"reason":"udp"`) && strings.Contains(logged, `"port":123`) {
			break
		}
	}
	_ = nw.Close()
	restore()
	_ = w.Close()
	out, _ := io.ReadAll(r)
	if !strings.Contains(logged, `"reason":"udp"`) || !strings.Contains(logged, `"port":123`) {
		t.Fatalf("prep-relay.log has no refusal of the NTP flow: %q", logged)
	}
	if len(out) != 0 {
		t.Fatalf("the relay wrote to the helper's stdout or stderr: %q", out)
	}
}

// TestPrepNetDialGuestPortRange: a port outside 1-65535 is refused, never
// narrowed into another port.
func TestPrepNetDialGuestPortRange(t *testing.T) {
	nw := wiredPrepNet(t, t.TempDir())
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	for _, port := range []int{0, 70000} {
		if _, err := nw.DialGuest(ctx, port); err == nil || !strings.Contains(err.Error(), "is not 1-65535") {
			t.Fatalf("DialGuest(%d): %v", port, err)
		}
	}
}

// TestPrepNetCloseTwice: prepare's deferred Close may follow another.
func TestPrepNetCloseTwice(t *testing.T) {
	nw := wiredPrepNet(t, t.TempDir())
	if err := nw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := nw.Close(); err != nil {
		t.Fatal(err)
	}
}
