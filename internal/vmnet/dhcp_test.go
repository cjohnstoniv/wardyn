// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"net"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

var testGuestMAC = tcpip.LinkAddress("\x52\x54\x00\x12\x34\x56")

// dhcpRequestFrame builds a guest's DHCP frame: msgType (53), plus any extra
// options already encoded.
func dhcpRequestFrame(msgType byte, xid uint32, extra ...byte) []byte {
	b := make([]byte, 240, 300)
	b[0], b[1], b[2] = 1, 1, 6
	b[4], b[5], b[6], b[7] = byte(xid>>24), byte(xid>>16), byte(xid>>8), byte(xid)
	copy(b[28:], testGuestMAC)
	copy(b[236:], dhcpCookie)
	b = append(b, 53, 1, msgType)
	b = append(b, extra...)
	b = append(b, 255)
	return udpBroadcastFrame(b)
}

func udpBroadcastFrame(payload []byte) []byte {
	total := header.EthernetMinimumSize + header.IPv4MinimumSize + header.UDPMinimumSize + len(payload)
	fr := make([]byte, total)
	header.Ethernet(fr).Encode(&header.EthernetFields{SrcAddr: testGuestMAC, DstAddr: broadcast, Type: header.IPv4ProtocolNumber})
	ip := header.IPv4(fr[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: uint16(total - header.EthernetMinimumSize), TTL: 64, Protocol: uint8(header.UDPProtocolNumber),
		SrcAddr: tcpip.AddrFrom4([4]byte{}), DstAddr: tcpip.AddrFrom4([4]byte{255, 255, 255, 255}),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	u := header.UDP(ip.Payload())
	u.Encode(&header.UDPFields{SrcPort: dhcpClientPort, DstPort: dhcpServerPort, Length: uint16(header.UDPMinimumSize + len(payload))})
	copy(u.Payload(), payload)
	return fr
}

// parsedReply is the fields of a DHCP reply the lease contract cares about.
type parsedReply struct {
	msgType byte
	yiaddr  [4]byte
	xid     []byte
	opts    map[byte][]byte
}

func parseReply(t *testing.T, fr []byte) parsedReply {
	t.Helper()
	if header.Ethernet(fr).DestinationAddress() != broadcast || header.Ethernet(fr).SourceAddress() != gatewayMAC {
		t.Fatalf("reply MACs %v -> %v", header.Ethernet(fr).SourceAddress(), header.Ethernet(fr).DestinationAddress())
	}
	ip := header.IPv4(fr[header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || !ip.IsChecksumValid() {
		t.Fatal("reply IPv4 header invalid")
	}
	u := header.UDP(ip.Payload())
	if u.SourcePort() != dhcpServerPort || u.DestinationPort() != dhcpClientPort {
		t.Fatalf("reply ports %d -> %d", u.SourcePort(), u.DestinationPort())
	}
	if !u.IsChecksumValid(ip.SourceAddress(), ip.DestinationAddress(), checksum.Checksum(u.Payload(), 0)) {
		t.Fatal("reply UDP checksum invalid")
	}
	b := u.Payload()
	if b[0] != 2 || !bytes.Equal(b[236:240], dhcpCookie) {
		t.Fatalf("not a BOOTREPLY with cookie: % x", b[:4])
	}
	r := parsedReply{xid: b[4:8], opts: map[byte][]byte{}}
	copy(r.yiaddr[:], b[16:20])
	for o := b[240:]; len(o) > 0 && o[0] != 255; o = o[2+int(o[1]):] {
		r.opts[o[0]] = o[2 : 2+int(o[1])]
	}
	r.msgType = r.opts[53][0]
	return r
}

func TestDHCPLease(t *testing.T) {
	lease := [4]byte{10, 0, 2, 15}
	gw := []byte{10, 0, 2, 2}

	t.Run("DISCOVER is offered exactly the link's lease", func(t *testing.T) {
		fr := dhcpRequestFrame(dhcpDiscover, 0xdeadbeef)
		if !isDHCPRequest(fr) {
			t.Fatal("not recognised as DHCP")
		}
		out, ok := dhcpReply(fr)
		if !ok {
			t.Fatal("no reply")
		}
		r := parseReply(t, out)
		if r.msgType != dhcpOffer || r.yiaddr != lease || !bytes.Equal(r.xid, []byte{0xde, 0xad, 0xbe, 0xef}) {
			t.Fatalf("offer %+v", r)
		}
		for opt, want := range map[byte][]byte{1: {255, 255, 255, 0}, 3: gw, 6: gw, 54: gw} {
			if !bytes.Equal(r.opts[opt], want) {
				t.Errorf("option %d = %v, want %v", opt, r.opts[opt], want)
			}
		}
		if len(r.opts[51]) != 4 {
			t.Errorf("no lease time: %v", r.opts)
		}
	})
	t.Run("REQUEST for the lease is acked, for any other address nak'd", func(t *testing.T) {
		out, ok := dhcpReply(dhcpRequestFrame(dhcpRequest, 1, 50, 4, 10, 0, 2, 15, 54, 4, 10, 0, 2, 2))
		if !ok || parseReply(t, out).msgType != dhcpAck || parseReply(t, out).yiaddr != lease {
			t.Fatalf("ack: %v %v", ok, out)
		}
		out, ok = dhcpReply(dhcpRequestFrame(dhcpRequest, 1, 50, 4, 10, 0, 2, 99))
		if !ok {
			t.Fatal("no nak")
		}
		if r := parseReply(t, out); r.msgType != dhcpNak || r.yiaddr != [4]byte{} {
			t.Fatalf("nak %+v", r)
		}
	})
	t.Run("what the responder does not answer", func(t *testing.T) {
		for name, fr := range map[string][]byte{
			"another server's REQUEST": dhcpRequestFrame(dhcpRequest, 1, 50, 4, 10, 0, 2, 15, 54, 4, 192, 168, 1, 1),
			"INFORM":                   dhcpRequestFrame(8, 1),
			"RELEASE":                  dhcpRequestFrame(7, 1),
			"truncated option":         dhcpRequestFrame(dhcpDiscover, 1, 12, 40, 'x'),
			"too short":                udpBroadcastFrame(make([]byte, 100)),
			"too long":                 udpBroadcastFrame(make([]byte, 2000)),
			"a reply, not a request":   udpBroadcastFrame(append([]byte{2, 1, 6}, make([]byte, 300)...)),
		} {
			if _, ok := dhcpReply(fr); ok {
				t.Errorf("%s was answered", name)
			}
		}
		// A relay agent's request is not ours to answer.
		fr := dhcpRequestFrame(dhcpDiscover, 1)
		fr[header.EthernetMinimumSize+header.IPv4MinimumSize+header.UDPMinimumSize+24] = 1
		if _, ok := dhcpReply(fr); ok {
			t.Error("a giaddr request was answered")
		}
	})
	t.Run("only IPv4 UDP to port 67 is the responder's", func(t *testing.T) {
		fr := dhcpRequestFrame(dhcpDiscover, 1)
		other := append([]byte(nil), fr...)
		header.UDP(header.IPv4(other[header.EthernetMinimumSize:]).Payload()).SetDestinationPort(53)
		if isDHCPRequest(other) || isDHCPRequest(fr[:20]) {
			t.Fatal("non-DHCP frame claimed")
		}
	})
}

// readReply reads one frame from the relay's side of a pipe, or fails after d.
func readReply(c net.Conn, d time.Duration) ([]byte, error) {
	_ = c.SetReadDeadline(time.Now().Add(d))
	return readFrame(c, make([]byte, maxFrame))
}

func TestServeAnswersDHCPOnlyWhenEnabled(t *testing.T) {
	serve := func(cfg Config) net.Conn {
		rl, err := New(cfg)
		if err != nil {
			t.Fatal(err)
		}
		a, b := net.Pipe()
		go func() { _ = rl.Serve(a) }()
		t.Cleanup(func() { _ = b.Close(); rl.Close() })
		return b
	}
	discover := dhcpRequestFrame(dhcpDiscover, 7)

	t.Run("enabled: a frame in, the offer back, through the length framing", func(t *testing.T) {
		c := serve(Config{DHCP: true})
		if err := writeFrame(c, discover); err != nil {
			t.Fatal(err)
		}
		fr, err := readReply(c, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if r := parseReply(t, fr); r.msgType != dhcpOffer || r.yiaddr != [4]byte{10, 0, 2, 15} {
			t.Fatalf("%+v", r)
		}
	})
	t.Run("unset (the Linux flow): no answer, the frame goes to the stack as UDP", func(t *testing.T) {
		c := serve(Config{})
		if err := writeFrame(c, discover); err != nil {
			t.Fatal(err)
		}
		if fr, err := readReply(c, 300*time.Millisecond); err == nil {
			t.Fatalf("unexpected frame back: % x", fr[:20])
		}
	})
}
