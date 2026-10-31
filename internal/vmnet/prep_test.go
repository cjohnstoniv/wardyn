// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/vmnet/vmnettest"
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

// lockedBuf is a log sink the relay writes to off the test goroutine.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// stackGuest is a guest at 10.0.2.15 with a stack of its own, so a test can
// listen on it and dial out of it, attached to serve the way a hypervisor
// would be.
func stackGuest(t *testing.T, serve func(net.Conn) error) *stack.Stack {
	t.Helper()
	relaySide, guestSide := net.Pipe()
	go func() { _ = serve(relaySide) }()
	link := channel.New(1024, linkMTU, testGuestMAC)
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
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, Gateway: gatewayIP, NIC: 1}})
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
		buf := make([]byte, maxFrame)
		for {
			frame, err := readFrame(guestSide, buf)
			if err != nil {
				return
			}
			pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(append([]byte(nil), frame...))})
			link.InjectInbound(header.Ethernet(frame).Type(), pkt)
			pkt.DecRef()
		}
	}()
	t.Cleanup(func() { cancel(); _ = guestSide.Close(); s.Close() })
	return s
}

func newPrepRelay(t *testing.T, logs io.Writer) *Relay {
	t.Helper()
	rl, err := New(Config{Prep: true, Log: slog.New(slog.NewJSONHandler(logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	return rl
}

func TestPrepDialGuest(t *testing.T) {
	rl := newPrepRelay(t, io.Discard)
	gs := stackGuest(t, rl.Serve)
	l, err := gonet.ListenTCP(gs, tcpip.FullAddress{NIC: 1, Addr: guestIP, Port: 22}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	go func() {
		c, err := l.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, "SSH-2.0-guest\r\n")
		_, _ = io.Copy(io.Discard, c)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := rl.DialGuest(ctx, 22)
	if err != nil {
		t.Fatalf("DialGuest: %v", err)
	}
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	line := make([]byte, len("SSH-2.0-guest\r\n"))
	if _, err := io.ReadFull(c, line); err != nil || string(line) != "SSH-2.0-guest\r\n" {
		t.Fatalf("banner %q, %v", line, err)
	}
	if got := c.RemoteAddr().String(); got != "10.0.2.15:22" {
		t.Fatalf("dialled %s", got)
	}
}

func TestDialGuestOnlyInPrep(t *testing.T) {
	rl, err := New(Config{Proxy: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	if _, err := rl.DialGuest(context.Background(), 22); err != ErrNotPrep {
		t.Fatalf("run relay DialGuest = %v, want ErrNotPrep", err)
	}
}

// A preparation relay answers every DNS query REFUSED and keeps no name; a run relay still sends the same
// queries to answer().
func TestPrepDNSIsRefusedAndRunDNSIsUnchanged(t *testing.T) {
	queries := []struct {
		labels []string
		qtype  dnsmessage.Type
	}{
		{[]string{"origin", "test"}, dnsmessage.TypeA},
		{[]string{"origin", "test"}, dnsmessage.TypeAAAA},
		{[]string{"origin", "test"}, dnsmessage.TypeMX},
		{[]string{"evil\r\nx", "test"}, dnsmessage.TypeA},
	}
	prep := newPrepRelay(t, io.Discard)
	pg := vmnettest.Attach(t, prep.Serve)
	run, err := New(Config{Proxy: "127.0.0.1:1"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(run.Close)
	rg := vmnettest.Attach(t, run.Serve)
	for _, q := range queries {
		m, err := pg.Query(q.labels, q.qtype)
		if err != nil || m.Header.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 {
			t.Fatalf("prep %v %v: reply %+v, %v; want REFUSED with no answer", q.labels, q.qtype, m, err)
		}
		// This proves run mode still goes to answer() (a fresh table, the same question). TestAnswer and
		// TestRelayRefusals are the pins on answer's bytes.
		wire := []byte{0x12, 0x34, 0x01, 0x00, 0, 1, 0, 0, 0, 0, 0, 0}
		for _, l := range q.labels {
			wire = append(append(wire, byte(len(l))), l...)
		}
		wire = append(wire, 0, byte(uint16(q.qtype)>>8), byte(q.qtype), 0, 1)
		want, _, err := answer(newNameTable(time.Now, maxNames), wire)
		if err != nil {
			t.Fatal(err)
		}
		got, err := rg.Query(q.labels, q.qtype)
		if err != nil {
			t.Fatalf("run %v %v: %v", q.labels, q.qtype, err)
		}
		gotWire, err := got.Pack()
		if err != nil {
			t.Fatal(err)
		}
		var wm dnsmessage.Message
		if err := wm.Unpack(want); err != nil {
			t.Fatal(err)
		}
		wantWire, _ := wm.Pack()
		if !bytes.Equal(gotWire, wantWire) {
			t.Fatalf("run %v %v: reply changed\n got %x\nwant %x", q.labels, q.qtype, gotWire, wantWire)
		}
	}
	if len(prep.names.byName) != 0 {
		t.Fatalf("the preparation relay's table holds %d names, want none", len(prep.names.byName))
	}
}

func TestPrepRefusesAllGuestEgress(t *testing.T) {
	var logs lockedBuf
	rl := newPrepRelay(t, &logs)
	gs := stackGuest(t, rl.Serve)

	// A literal address, port 80, 443 and an arbitrary port: each is
	// reset at the SYN; none can complete.
	for _, target := range []struct {
		addr [4]byte
		port uint16
	}{{[4]byte{1, 1, 1, 1}, 443}, {[4]byte{93, 184, 216, 34}, 80}, {[4]byte{10, 0, 2, 2}, 3128}, {[4]byte{169, 254, 169, 254}, 80}, {[4]byte{198, 18, 0, 1}, 443}} {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		c, err := gonet.DialContextTCP(ctx, gs, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(target.addr), Port: target.port}, ipv4.ProtocolNumber)
		cancel()
		if err == nil {
			c.Close()
			t.Fatalf("guest connected to %v:%d through a preparation relay", target.addr, target.port)
		}
	}
	waitFor(t, "prep-egress refusal logged", func() bool { return strings.Contains(logs.String(), `"reason":"prep-egress"`) })
	if strings.Contains(logs.String(), "tcp_allow") {
		t.Fatalf("an allow in a preparation relay: %s", logs.String())
	}
}
