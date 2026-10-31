// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"encoding/binary"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/checksum"
	"gvisor.dev/gvisor/pkg/tcpip/header"
)

// A macOS guest configures its NIC with DHCP, where the Linux image is static
// (relay.go). The responder below answers a guest's DHCPv4 DISCOVER and
// REQUEST with the one lease the relay's link already is: 10.0.2.15/24, router,
// resolver and server 10.0.2.2. It reads a bounded, fixed-layout request and
// builds a fixed reply; it is on only when Config.DHCP is set, so a Linux
// guest's frames reach the stack exactly as before.
const (
	dhcpServerPort = 67
	dhcpClientPort = 68
	// A BOOTP request is 236 bytes, then the magic cookie; clients pad to 300
	// and a long options list stays well under this bound.
	dhcpMinPayload = 240
	dhcpMaxPayload = 576
	dhcpLeaseSecs  = 86400

	dhcpDiscover = 1
	dhcpOffer    = 2
	dhcpRequest  = 3
	dhcpDecline  = 4
	dhcpAck      = 5
	dhcpNak      = 6
)

var (
	guestIP    = tcpip.AddrFrom4([4]byte{10, 0, 2, 15})
	dhcpCookie = []byte{99, 130, 83, 99}
	broadcast  = tcpip.LinkAddress("\xff\xff\xff\xff\xff\xff")
)

// isDHCPRequest reports whether the frame is IPv4/UDP to the DHCP server port.
// Such a frame is the responder's and never reaches the stack.
func isDHCPRequest(frame []byte) bool {
	if len(frame) < header.EthernetMinimumSize || header.Ethernet(frame).Type() != header.IPv4ProtocolNumber {
		return false
	}
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	if !ip.IsValid(len(ip)) || ip.TransportProtocol() != header.UDPProtocolNumber {
		return false
	}
	if ip.FragmentOffset() != 0 || ip.More() {
		return false
	}
	p := ip.Payload()
	return len(p) >= header.UDPMinimumSize && header.UDP(p).DestinationPort() == dhcpServerPort
}

// dhcpMessage is a guest's DHCP request: its BOOTP payload and the options the
// responder reads.
type dhcpMessage struct {
	b                        []byte
	msgType                  byte
	reqIP, serverID, message []byte
}

// parseDHCP reads one DHCP request frame; ok is false for a malformed one.
func parseDHCP(frame []byte) (m dhcpMessage, ok bool) {
	ip := header.IPv4(frame[header.EthernetMinimumSize:])
	p := ip.Payload()
	udp := header.UDP(p)
	if int(udp.Length()) > len(p) || udp.Length() < header.UDPMinimumSize {
		return m, false
	}
	b := p[header.UDPMinimumSize:udp.Length()]
	if len(b) < dhcpMinPayload || len(b) > dhcpMaxPayload {
		return m, false
	}
	// op=BOOTREQUEST, htype=Ethernet, hlen=6, giaddr=0 (no relay agent).
	if b[0] != 1 || b[1] != 1 || b[2] != 6 || !bytes.Equal(b[24:28], []byte{0, 0, 0, 0}) || !bytes.Equal(b[236:240], dhcpCookie) {
		return m, false
	}
	m.b = b
	opts := b[240:]
	for len(opts) > 0 {
		code := opts[0]
		if code == 255 {
			break
		}
		if code == 0 {
			opts = opts[1:]
			continue
		}
		if len(opts) < 2 || len(opts) < 2+int(opts[1]) {
			return m, false
		}
		val := opts[2 : 2+int(opts[1])]
		switch code {
		case 53:
			if len(val) != 1 {
				return m, false
			}
			m.msgType = val[0]
		case 50:
			m.reqIP = val
		case 54:
			m.serverID = val
		case 56:
			m.message = val
		}
		opts = opts[2+int(opts[1]):]
	}
	return m, true
}

// dhcpDeclined reports whether frame is the guest's DECLINE of a lease, the
// address it declined (option 50) and the reason it gave (option 56), if any. A
// guest declines when the address it was offered answers its probe as in use
// (RFC 5227).
func dhcpDeclined(frame []byte) (addr, message string, ok bool) {
	m, ok := parseDHCP(frame)
	if !ok || m.msgType != dhcpDecline {
		return "", "", false
	}
	if len(m.reqIP) == 4 {
		addr = tcpip.AddrFrom4Slice(m.reqIP).String()
	}
	return addr, string(m.message), true
}

// dhcpReply answers one DHCP request frame; ok is false for anything it does
// not answer (malformed, not a DISCOVER or REQUEST, a REQUEST for another
// server).
func dhcpReply(frame []byte) (reply []byte, ok bool) {
	m, ok := parseDHCP(frame)
	if !ok {
		return nil, false
	}
	b, reqIP, serverID := m.b, m.reqIP, m.serverID
	rt := byte(dhcpOffer)
	switch m.msgType {
	case dhcpDiscover:
	case dhcpRequest:
		// A REQUEST that names another server is that server's, not ours.
		if len(serverID) > 0 && !bytes.Equal(serverID, gatewayIP.AsSlice()) {
			return nil, false
		}
		want := reqIP
		if len(want) == 0 {
			want = b[12:16] // renewal: ciaddr
		}
		rt = dhcpAck
		if !bytes.Equal(want, guestIP.AsSlice()) {
			rt = dhcpNak
		}
	default:
		return nil, false
	}

	out := make([]byte, 240, 300)
	out[0], out[1], out[2] = 2, 1, 6 // BOOTREPLY, Ethernet, hlen 6
	copy(out[4:8], b[4:8])           // xid
	copy(out[10:12], b[10:12])       // flags
	if rt != dhcpNak {
		copy(out[16:20], guestIP.AsSlice()) // yiaddr
	}
	copy(out[20:24], gatewayIP.AsSlice()) // siaddr
	copy(out[28:44], b[28:44])            // chaddr
	copy(out[236:240], dhcpCookie)
	out = append(out, 53, 1, rt, 54, 4)
	out = append(out, gatewayIP.AsSlice()...)
	if rt != dhcpNak {
		out = append(out, 51, 4)
		out = binary.BigEndian.AppendUint32(out, dhcpLeaseSecs)
		out = append(out, 1, 4, 255, 255, 255, 0)
		out = append(out, 3, 4)
		out = append(out, gatewayIP.AsSlice()...)
		out = append(out, 6, 4)
		out = append(out, gatewayIP.AsSlice()...)
	}
	out = append(out, 255)

	// Broadcast reply: the guest has no address to unicast to until it takes
	// the lease.
	total := header.EthernetMinimumSize + header.IPv4MinimumSize + header.UDPMinimumSize + len(out)
	fr := make([]byte, total)
	header.Ethernet(fr).Encode(&header.EthernetFields{SrcAddr: gatewayMAC, DstAddr: broadcast, Type: header.IPv4ProtocolNumber})
	ipo := header.IPv4(fr[header.EthernetMinimumSize:])
	ipo.Encode(&header.IPv4Fields{
		TotalLength: uint16(total - header.EthernetMinimumSize),
		TTL:         64,
		Protocol:    uint8(header.UDPProtocolNumber),
		SrcAddr:     gatewayIP,
		DstAddr:     tcpip.AddrFrom4([4]byte{255, 255, 255, 255}),
	})
	ipo.SetChecksum(^ipo.CalculateChecksum())
	u := header.UDP(ipo.Payload())
	u.Encode(&header.UDPFields{SrcPort: dhcpServerPort, DstPort: dhcpClientPort, Length: uint16(header.UDPMinimumSize + len(out))})
	copy(u.Payload(), out)
	sum := header.PseudoHeaderChecksum(header.UDPProtocolNumber, ipo.SourceAddress(), ipo.DestinationAddress(), u.Length())
	u.SetChecksum(^u.CalculateChecksum(checksum.Checksum(u.Payload(), sum)))
	return fr, true
}
