// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

//go:build unix

package vmnet

import (
	"net"
	"strings"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// arpRequestFrame is the guest's who-has for target, sent from sender
// (0.0.0.0 for an RFC 5227 probe).
func arpRequestFrame(sender, target tcpip.Address) []byte {
	fr := make([]byte, header.EthernetMinimumSize+header.ARPSize)
	header.Ethernet(fr).Encode(&header.EthernetFields{SrcAddr: testGuestMAC, DstAddr: broadcast, Type: header.ARPProtocolNumber})
	a := header.ARP(fr[header.EthernetMinimumSize:])
	a.SetIPv4OverEthernet()
	a.SetOp(header.ARPRequest)
	copy(a.HardwareAddressSender(), testGuestMAC)
	copy(a.ProtocolAddressSender(), sender.AsSlice())
	copy(a.ProtocolAddressTarget(), target.AsSlice())
	return fr
}

// A macOS guest probes the address DHCP offered before it takes it, and
// declines the lease if anything answers. The relay answers who-has for the
// gateway only, so the probe (and any other on-link address) goes unanswered.
func TestRelayAnswersARPOnlyForTheGateway(t *testing.T) {
	rl, _ := prepRelay(t)
	a, b := net.Pipe()
	go func() { _ = rl.Serve(a) }()
	t.Cleanup(func() { _ = b.Close(); rl.Close() })

	unspecified := tcpip.AddrFrom4([4]byte{})
	for _, fr := range [][]byte{
		arpRequestFrame(unspecified, guestIP),                            // the probe of the offered address
		arpRequestFrame(guestIP, guestIP),                                // the announcement once it is taken
		arpRequestFrame(guestIP, tcpip.AddrFrom4([4]byte{10, 0, 2, 99})), // another on-link address
		arpRequestFrame(guestIP, gatewayIP),
	} {
		if err := writeFrame(b, fr); err != nil {
			t.Fatal(err)
		}
	}
	// The pipe keeps order: the first frame back is the answer to the first
	// request the relay answered.
	fr, err := readReply(b, 3*time.Second)
	if err != nil {
		t.Fatalf("no answer to the gateway who-has: %v", err)
	}
	if header.Ethernet(fr).Type() != header.ARPProtocolNumber {
		t.Fatalf("first frame back is ethertype %#x, not ARP", header.Ethernet(fr).Type())
	}
	reply := header.ARP(fr[header.EthernetMinimumSize:])
	if got := tcpip.AddrFrom4Slice(reply.ProtocolAddressSender()); reply.Op() != header.ARPReply || got != gatewayIP {
		t.Fatalf("first ARP back is op %d for %s; want the gateway's reply (an earlier request was answered)", reply.Op(), got)
	}
	if fr, err := readReply(b, 300*time.Millisecond); err == nil {
		t.Fatalf("unexpected frame after the gateway's reply: % x", fr[:min(len(fr), 42)])
	}
}

// A DECLINE is the guest saying the offered address is in use: the relay logs
// it as that, with the guest's reason, and not as an unanswered request.
func TestRelayLogsADHCPDecline(t *testing.T) {
	rl, log := prepRelay(t)
	a, b := net.Pipe()
	go func() { _ = rl.Serve(a) }()
	t.Cleanup(func() { _ = b.Close(); rl.Close() })

	reason := "address in use"
	opt := append([]byte{56, byte(len(reason))}, reason...)
	if err := writeFrame(b, dhcpRequestFrame(dhcpDecline, 9, opt...)); err != nil {
		t.Fatal(err)
	}
	if fr, err := readReply(b, 300*time.Millisecond); err == nil {
		t.Fatalf("a DECLINE was answered: % x", fr[:min(len(fr), 42)])
	}
	rl.rep.flush()
	got := log.String()
	if !strings.Contains(got, "declined the DHCP lease") || !strings.Contains(got, reason) {
		t.Fatalf("log has no DECLINE line with the guest's reason:\n%s", got)
	}
	if strings.Contains(got, "dhcp_unanswered") {
		t.Fatalf("a DECLINE was logged as unanswered:\n%s", got)
	}
}
