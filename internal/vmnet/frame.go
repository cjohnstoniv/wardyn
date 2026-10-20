// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// QEMU's -netdev stream framing: a 4-byte big-endian length, then one Ethernet
// frame. The length comes from the far side of the socket — QEMU, which is the
// process a guest escape would control — so it is bounded: 64 KiB of payload
// plus the Ethernet header is the most virtio-net ever sends, and anything
// larger or shorter than a header ends the connection.
const (
	maxFrame = 65536 + header.EthernetMinimumSize
	minFrame = header.EthernetMinimumSize
)

var errFrameLength = errors.New("vmnet: frame length out of bounds")

// readFrame reads one frame into buf (which must hold maxFrame bytes).
func readFrame(r io.Reader, buf []byte) ([]byte, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n < minFrame || n > maxFrame {
		return nil, fmt.Errorf("%w: %d", errFrameLength, n)
	}
	if _, err := io.ReadFull(r, buf[:n]); err != nil {
		return nil, err
	}
	return buf[:n], nil
}

// writeFrame writes one length-prefixed frame.
func writeFrame(w io.Writer, frame []byte) error {
	out := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(out, uint32(len(frame)))
	copy(out[4:], frame)
	_, err := w.Write(out)
	return err
}

// filterFrame is the frame filter in front of the stack: ARP, IPv4/TCP and
// IPv4/UDP pass; everything else (ICMP, IPv6, VLAN tags, RARP, any other
// ethertype or IPv4 protocol, a malformed IPv4 header) is dropped, and kind
// names it for the drop report from a closed set, so a guest spraying
// ethertypes cannot grow the rate limiter's table.
func filterFrame(frame []byte) (pass bool, kind string) {
	eth := header.Ethernet(frame)
	switch t := eth.Type(); t {
	case header.ARPProtocolNumber:
		return true, ""
	case header.IPv4ProtocolNumber:
		ip := header.IPv4(frame[header.EthernetMinimumSize:])
		if !ip.IsValid(len(ip)) {
			return false, "ipv4_malformed"
		}
		switch ip.TransportProtocol() {
		case header.TCPProtocolNumber, header.UDPProtocolNumber:
			return true, ""
		case header.ICMPv4ProtocolNumber:
			return false, "icmp"
		}
		return false, "ipv4_other"
	case header.IPv6ProtocolNumber:
		return false, "ipv6"
	case 0x8100, 0x88a8:
		return false, "vlan"
	case 0x8035:
		return false, "rarp"
	default:
		return false, "other"
	}
}
