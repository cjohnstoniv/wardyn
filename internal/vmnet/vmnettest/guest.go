// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package vmnettest attaches a user-space guest to a vmnet relay the way QEMU
// does (a stream of length-prefixed Ethernet frames), so the relay and its
// contract with the proxy can be tested without a VM.
package vmnettest

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"
	"gvisor.dev/gvisor/pkg/buffer"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/link/channel"
	"gvisor.dev/gvisor/pkg/tcpip/link/ethernet"
	"gvisor.dev/gvisor/pkg/tcpip/network/arp"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
	"gvisor.dev/gvisor/pkg/tcpip/transport/tcp"
	"gvisor.dev/gvisor/pkg/tcpip/transport/udp"
)

var (
	guestMAC = tcpip.LinkAddress("\x52\x54\x00\x12\x34\x56")
	guestIP  = tcpip.AddrFrom4([4]byte{10, 0, 2, 15})
	// Gateway is the relay's address on the guest link.
	Gateway = [4]byte{10, 0, 2, 2}
)

// Guest is one attached guest.
type Guest struct {
	s    *stack.Stack
	conn net.Conn
}

// Attach starts serve (a relay's Serve) on one end of a pipe and a guest
// stack on the other, both stopped at test cleanup.
func Attach(t testing.TB, serve func(net.Conn) error) *Guest {
	t.Helper()
	relaySide, guestSide := net.Pipe()
	go func() { _ = serve(relaySide) }()
	link := channel.New(1024, 1500, guestMAC)
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	if err := s.CreateNIC(1, ethernet.New(link)); err != nil {
		t.Fatal(err)
	}
	if err := s.AddProtocolAddress(1, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: guestIP, PrefixLen: 24}}, stack.AddressProperties{}); err != nil {
		t.Fatal(err)
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, Gateway: tcpip.AddrFrom4(Gateway), NIC: 1}})
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		for {
			pkt := link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			err := writeFrame(guestSide, pkt.ToView().AsSlice())
			pkt.DecRef()
			if err != nil {
				return
			}
		}
	}()
	go func() {
		var hdr [4]byte
		for {
			if _, err := io.ReadFull(guestSide, hdr[:]); err != nil {
				return
			}
			frame := make([]byte, binary.BigEndian.Uint32(hdr[:]))
			if _, err := io.ReadFull(guestSide, frame); err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
			link.InjectInbound(header.Ethernet(frame).Type(), pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = guestSide.Close()
		s.Close()
	})
	return &Guest{s: s, conn: guestSide}
}

func writeFrame(w io.Writer, frame []byte) error {
	out := make([]byte, 4+len(frame))
	binary.BigEndian.PutUint32(out, uint32(len(frame)))
	copy(out[4:], frame)
	_, err := w.Write(out)
	return err
}

// Query sends one DNS question with the raw name bytes given (so a test can
// put CR or LF inside a label) and returns the reply, or an error after 2 s.
func (g *Guest) Query(labels []string, qtype dnsmessage.Type) (*dnsmessage.Message, error) {
	c, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(Gateway), Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		return nil, errors.New(err.Error())
	}
	defer c.Close()
	q := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
	for _, l := range labels {
		q = append(q, byte(len(l)))
		q = append(q, l...)
	}
	q = append(q, 0, byte(uint16(qtype)>>8), byte(qtype), 0, 1)
	if _, err := c.Write(q); err != nil {
		return nil, err
	}
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 1500)
	n, err := c.Read(buf)
	if err != nil {
		return nil, err
	}
	var m dnsmessage.Message
	if err := m.Unpack(buf[:n]); err != nil {
		return nil, err
	}
	return &m, nil
}

// Resolve returns the A answer for a dotted name, failing the test without one.
func (g *Guest) Resolve(t testing.TB, labels ...string) [4]byte {
	t.Helper()
	m, err := g.Query(labels, dnsmessage.TypeA)
	if err != nil {
		t.Fatalf("resolve %v: %v", labels, err)
	}
	for _, a := range m.Answers {
		if r, ok := a.Body.(*dnsmessage.AResource); ok {
			return r.A
		}
	}
	t.Fatalf("resolve %v: no A answer (rcode %v)", labels, m.Header.RCode)
	return [4]byte{}
}

// Dial opens a guest TCP connection to addr:port.
func (g *Guest) Dial(addr [4]byte, port uint16, timeout time.Duration) (*gonet.TCPConn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return gonet.DialContextTCP(ctx, g.s, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(addr), Port: port}, ipv4.ProtocolNumber)
}

// SendUDP sends one datagram from the guest.
func (g *Guest) SendUDP(addr [4]byte, port uint16, payload []byte) error {
	c, err := gonet.DialUDP(g.s, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(addr), Port: port}, ipv4.ProtocolNumber)
	if err != nil {
		return errors.New(err.Error())
	}
	defer c.Close()
	_, werr := c.Write(payload)
	return werr
}

// SendRaw writes one raw length-prefixed frame of the given ethertype, as a
// guest with root (or a compromised QEMU) could.
func (g *Guest) SendRaw(ethertype uint16, payload []byte) error {
	frame := make([]byte, header.EthernetMinimumSize+len(payload))
	copy(frame[0:6], "\x5a\x94\xef\xe4\x0c\xdd")
	copy(frame[6:12], guestMAC)
	binary.BigEndian.PutUint16(frame[12:14], ethertype)
	copy(frame[14:], payload)
	return writeFrame(g.conn, frame)
}

// SendLength writes a bare frame-length header, for the relay's bound check.
func (g *Guest) SendLength(n uint32) error {
	var hdr [4]byte
	binary.BigEndian.PutUint32(hdr[:], n)
	_, err := g.conn.Write(hdr[:])
	return err
}
