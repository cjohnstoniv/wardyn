// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

// What a guest on a -stdio relay can reach, as unit-level REACH (the Mac's
// P7 is the same list against a real guest): the run proxy, by name, and the
// relay's own DNS. Everything else is reset or dropped at the relay and never
// reaches the proxy, whatever the proxy would have said to it.
type reachTarget struct {
	name string
	ip   [4]byte
	port uint16
}

var reachTCP = []reachTarget{
	{"host LAN ssh", [4]byte{192, 168, 1, 10}, 22},
	{"host LAN gateway https", [4]byte{192, 168, 1, 1}, 443},
	{"relay address ssh", [4]byte{10, 0, 2, 2}, 22},
	{"relay address http", [4]byte{10, 0, 2, 2}, 80},
	{"relay address proxy port", [4]byte{10, 0, 2, 2}, 3128},
	{"relay address dns over tcp", [4]byte{10, 0, 2, 2}, 53},
	{"metadata http", [4]byte{169, 254, 169, 254}, 80},
	{"metadata https", [4]byte{169, 254, 169, 254}, 443},
	{"public dns over tcp", [4]byte{1, 1, 1, 1}, 53},
	{"public https", [4]byte{1, 1, 1, 1}, 443},
}

var reachUDP = []reachTarget{
	{"public dns", [4]byte{1, 1, 1, 1}, 53},
	{"public dns 2", [4]byte{8, 8, 8, 8}, 53},
	{"relay address ntp", [4]byte{10, 0, 2, 2}, 123},
	{"metadata dns", [4]byte{169, 254, 169, 254}, 53},
	{"host LAN dns", [4]byte{192, 168, 1, 1}, 53},
}

func dialTCP(gs *stack.Stack, ip [4]byte, port uint16) (net.Conn, error) {
	return dialTCPWithin(gs, ip, port, time.Second)
}

// dialTCPWithin is dialTCP with the caller's deadline. A refusal (reset)
// returns at once whatever the deadline; the deadline only bounds a dial that
// is still waiting, so a long one waits out a slow runner without letting a
// refused target look reachable.
func dialTCPWithin(gs *stack.Stack, ip [4]byte, port uint16, d time.Duration) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	return gonet.DialContextTCP(ctx, gs, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(ip), Port: port}, ipv4.ProtocolNumber)
}

func ipPort(t reachTarget) string {
	return fmt.Sprintf("%d.%d.%d.%d:%d", t.ip[0], t.ip[1], t.ip[2], t.ip[3], t.port)
}

// dnsQuery asks the relay's resolver for name over the guest stack and returns
// the A answer.
func dnsQuery(t *testing.T, gs *stack.Stack, label, tld string) [4]byte {
	t.Helper()
	c, err := gonet.DialUDP(gs, nil, &tcpip.FullAddress{NIC: 1, Addr: gatewayIP, Port: 53}, ipv4.ProtocolNumber)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	q := []byte{0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0, byte(len(label))}
	q = append(q, label...)
	q = append(q, byte(len(tld)))
	q = append(q, tld...)
	q = append(q, 0, 0, 1, 0, 1)
	_, _ = c.Write(q)
	// A bound for a hung relay, not a latency budget: see vmnettest.Query.
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	buf := make([]byte, 512)
	n, err := c.Read(buf)
	if err != nil || n < 4+len(q) {
		t.Fatalf("dns reply %d bytes, %v", n, err)
	}
	return [4]byte(buf[n-4 : n])
}

func TestStdioRelayReach(t *testing.T) {
	// The proxy would allow every one of these literal targets: a relay that
	// forwards what the guest asks for reaches the proxy and the guest connects.
	allowed := []string{"allowed.test:443"}
	for _, tg := range reachTCP {
		allowed = append(allowed, ipPort(tg))
	}
	f := startFakeProxy(t, allowed...)
	rl, err := New(Config{Proxy: f.addr, DHCP: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	gs := stackGuest(t, func(c net.Conn) error { return rl.ServeStream(c, c) })

	for _, tg := range reachTCP {
		if c, err := dialTCP(gs, tg.ip, tg.port); err == nil {
			c.Close()
			t.Errorf("REACH: guest TCP connected to %s (%s)", ipPort(tg), tg.name)
		}
	}
	for _, tg := range reachUDP {
		c, err := gonet.DialUDP(gs, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(tg.ip), Port: tg.port}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte{0x12, 0x34, 0x01, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 1, 0, 1})
		_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
		if n, err := c.Read(make([]byte, 512)); err == nil {
			t.Errorf("REACH: guest UDP got %d bytes back from %s (%s)", n, ipPort(tg), tg.name)
		}
		c.Close()
	}
	if ev, _ := f.snapshot(); len(ev) != 0 {
		t.Errorf("REACH: the proxy was asked about literal targets: %q", ev)
	}
	waitFor(t, "udp refusals reported", refusedWith(f, "udp"))

	// The two things that are reachable: the relay's DNS, and a proxy-allowed name.
	addr := dnsQuery(t, gs, "allowed", "test")
	// The one positive dial gets a generous deadline: under -race on a hosted
	// runner the first connect through the relay can outlast the negatives'
	// one second. Every negative above keeps dialTCP's short deadline.
	c, err := dialTCPWithin(gs, addr, 443, 10*time.Second)
	if err != nil {
		t.Fatalf("a proxy-allowed name is unreachable: %v", err)
	}
	c.Close()
}

// A preparation relay's scope is smaller: no proxy, so the guest reaches
// nothing at all (a DNS name gets a refusal: TestPrepDNSIsRefusedAndRunDNSIsUnchanged), and the only path is the host's
// DialGuest to the guest's own address. A dial to the proxy address from a
// preparation relay is a failure of that scope.
func TestPrepRelayReach(t *testing.T) {
	var dials atomic.Int32
	rl, err := New(Config{Prep: true, Log: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Dial: func(context.Context, string, string) (net.Conn, error) {
			dials.Add(1)
			return nil, io.ErrClosedPipe
		}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	gs := stackGuest(t, rl.Serve)

	for _, tg := range reachTCP {
		if c, err := dialTCP(gs, tg.ip, tg.port); err == nil {
			c.Close()
			t.Errorf("REACH: prep guest TCP connected to %s (%s)", ipPort(tg), tg.name)
		}
	}
	for _, tg := range reachUDP {
		c, err := gonet.DialUDP(gs, nil, &tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(tg.ip), Port: tg.port}, ipv4.ProtocolNumber)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = c.Write([]byte{1, 2, 3})
		_ = c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if n, err := c.Read(make([]byte, 512)); err == nil {
			t.Errorf("REACH: prep guest UDP got %d bytes back from %s", n, ipPort(tg))
		}
		c.Close()
	}
	time.Sleep(200 * time.Millisecond) // reports and asks run off the guest's path
	if n := dials.Load(); n != 0 {
		t.Errorf("REACH: a preparation relay made %d outbound dials; it has no egress", n)
	}
}
