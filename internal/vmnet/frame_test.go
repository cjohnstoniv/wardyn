// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

func ipv4Frame(proto uint8) []byte {
	frame := make([]byte, header.EthernetMinimumSize+header.IPv4MinimumSize)
	binary.BigEndian.PutUint16(frame[12:], uint16(header.IPv4ProtocolNumber))
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	ip.Encode(&header.IPv4Fields{
		TotalLength: header.IPv4MinimumSize, TTL: 64, Protocol: proto,
		SrcAddr: tcpip.AddrFrom4([4]byte{10, 0, 2, 15}), DstAddr: tcpip.AddrFrom4([4]byte{8, 8, 8, 8}),
	})
	ip.SetChecksum(^ip.CalculateChecksum())
	return frame
}

func etherFrame(t uint16) []byte {
	frame := make([]byte, header.EthernetMinimumSize+28)
	binary.BigEndian.PutUint16(frame[12:], t)
	return frame
}

func TestFilterFrame(t *testing.T) {
	for _, c := range []struct {
		name  string
		frame []byte
		pass  bool
		kind  string
	}{
		{"arp", etherFrame(0x0806), true, ""},
		{"tcp", ipv4Frame(6), true, ""},
		{"udp", ipv4Frame(17), true, ""},
		{"icmp", ipv4Frame(1), false, "icmp"},
		{"gre", ipv4Frame(47), false, "ipv4_other"},
		{"ipv6", etherFrame(0x86dd), false, "ipv6"},
		{"vlan", etherFrame(0x8100), false, "vlan"},
		{"rarp", etherFrame(0x8035), false, "rarp"},
		{"other", etherFrame(0x1234), false, "other"},
		{"malformed ipv4", etherFrame(0x0800), false, "ipv4_malformed"},
	} {
		if pass, kind := filterFrame(c.frame); pass != c.pass || kind != c.kind {
			t.Errorf("%s: pass %v kind %q, want %v %q", c.name, pass, kind, c.pass, c.kind)
		}
	}
}

func TestReadFrameBounds(t *testing.T) {
	buf := make([]byte, maxFrame)
	for _, n := range []uint32{0, minFrame - 1, maxFrame + 1, 1 << 31} {
		var in bytes.Buffer
		_ = binary.Write(&in, binary.BigEndian, n)
		if _, err := readFrame(&in, buf); !errors.Is(err, errFrameLength) {
			t.Errorf("length %d: err %v, want errFrameLength", n, err)
		}
	}
	var in bytes.Buffer
	_ = writeFrame(&in, etherFrame(0x0806))
	f, err := readFrame(&in, buf)
	if err != nil || len(f) != header.EthernetMinimumSize+28 {
		t.Fatalf("round trip: %d bytes, %v", len(f), err)
	}
}
