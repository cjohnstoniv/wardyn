// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Package vmnet is wardyn-vmnet, a desktop VM's network relay: every connection
// the desktop makes goes through it, and it hands allowed ones to the run's
// proxy and drops the rest.
//
// QEMU's virtio-net backend (-netdev stream) connects to a unix socket the relay
// serves, so the guest's Ethernet frames land in a user-space TCP/IP stack
// (gVisor netstack) and nowhere else. The relay answers ARP and DNS itself:
// a name gets a synthetic address (names.go) and nothing is looked up upstream.
// Each guest TCP flow to a synthetic address is put to the proxy as a CONNECT
// named by that DNS answer, asked before the guest's handshake completes (a
// refusal is a reset, an approval hold is a held SYN) and tunnelled only after
// the first flight passed the TLS server-name check (firstflight.go); port 80
// goes as forward-proxy requests (httpfwd.go). A flow to any other address, UDP
// other than DNS, and every frame that is not ARP, IPv4/TCP or IPv4/UDP are
// dropped and reported (report.go). The relay holds no token and no policy: the
// proxy decides, and its decision log carries the relay's refusals too.
package vmnet

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"time"

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
	"gvisor.dev/gvisor/pkg/waiter"
)

// The link the guest sees: it is 10.0.2.15/24 (static, from its kernel command
// line); the relay is its gateway and resolver at 10.0.2.2.
var (
	gatewayMAC = tcpip.LinkAddress("\x5a\x94\xef\xe4\x0c\xdd")
	gatewayIP  = tcpip.AddrFrom4([4]byte{10, 0, 2, 2})
)

const (
	nicID       = 1
	linkMTU     = 1500
	maxInFlight = 1024 // half-open guest SYNs the stack holds at once
)

// Config is how a relay reaches its proxy.
type Config struct {
	// Proxy is the run's wardyn-proxy, host:port.
	Proxy string
	// Log receives one JSON line per DNS addition, eviction, allow and
	// refusal (refusals rate limited per kind).
	Log *slog.Logger
	// Dial and Now are seams for tests; nil means net.Dialer and time.Now.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
	Now  func() time.Time
}

// Relay is one VM's relay. It serves one QEMU connection at a time and keeps
// its name table across QEMU reconnects.
type Relay struct {
	stack *stack.Stack
	link  *channel.Endpoint
	names *nameTable
	proxy *proxyClient
	rep   *reporter
	log   *slog.Logger
	stop  chan struct{}
}

// New builds a relay's stack. Close releases it.
func New(cfg Config) (*Relay, error) {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	log := cfg.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	rl := &Relay{
		link:  channel.New(1024, linkMTU, gatewayMAC),
		names: newNameTable(now, maxNames),
		proxy: newProxyClient(cfg.Proxy, cfg.Dial),
		log:   log,
		stop:  make(chan struct{}),
	}
	rl.rep = newReporter(now, log, func(r refusal) {
		if err := rl.proxy.send(r); err != nil {
			log.Warn("refusal report failed", "reason", r.Reason, "err", err.Error())
		}
	})
	s := stack.New(stack.Options{
		NetworkProtocols:   []stack.NetworkProtocolFactory{ipv4.NewProtocol, arp.NewProtocol},
		TransportProtocols: []stack.TransportProtocolFactory{tcp.NewProtocol, udp.NewProtocol},
	})
	rl.stack = s
	if err := s.CreateNIC(nicID, ethernet.New(rl.link)); err != nil {
		return nil, errors.New(err.String())
	}
	if err := s.AddProtocolAddress(nicID, tcpip.ProtocolAddress{Protocol: ipv4.ProtocolNumber,
		AddressWithPrefix: tcpip.AddressWithPrefix{Address: gatewayIP, PrefixLen: 24}}, stack.AddressProperties{}); err != nil {
		return nil, errors.New(err.String())
	}
	s.SetRouteTable([]tcpip.Route{{Destination: header.IPv4EmptySubnet, NIC: nicID}})
	// Promiscuous + spoofing: the stack accepts a guest flow to ANY address and
	// answers as it, so the forwarders below see every flow and decide on it.
	if err := s.SetPromiscuousMode(nicID, true); err != nil {
		return nil, errors.New(err.String())
	}
	if err := s.SetSpoofing(nicID, true); err != nil {
		return nil, errors.New(err.String())
	}
	s.SetTransportProtocolHandler(tcp.ProtocolNumber, tcp.NewForwarder(s, 0, maxInFlight, rl.handleTCP).HandlePacket)
	s.SetTransportProtocolHandler(udp.ProtocolNumber, udp.NewForwarder(s, func(r *udp.ForwarderRequest) bool {
		id := r.ID()
		rl.rep.refuse(refusal{Reason: "udp", Host: id.LocalAddress.String(), Port: int(id.LocalPort)})
		return true // swallowed: no endpoint, no reply
	}).HandlePacket)
	dnsConn, err := gonet.DialUDP(s, &tcpip.FullAddress{NIC: nicID, Addr: gatewayIP, Port: 53}, nil, ipv4.ProtocolNumber)
	if err != nil {
		return nil, err
	}
	go rl.serveDNS(dnsConn)
	go func() {
		t := time.NewTicker(reportInterval)
		defer t.Stop()
		for {
			select {
			case <-rl.stop:
				return
			case <-t.C:
				rl.rep.flush()
			}
		}
	}()
	return rl, nil
}

// Close stops the relay.
func (rl *Relay) Close() {
	close(rl.stop)
	rl.stack.Close()
}

// Serve carries one QEMU connection's frames until it ends. Open guest flows
// survive a reconnect only as long as the guest's own retries do.
func (rl *Relay) Serve(c net.Conn) error {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		for {
			pkt := rl.link.ReadContext(ctx)
			if pkt == nil {
				return
			}
			frame := pkt.ToView().AsSlice()
			err := writeFrame(c, frame)
			pkt.DecRef()
			if err != nil {
				return
			}
		}
	}()
	buf := make([]byte, maxFrame)
	for {
		frame, err := readFrame(c, buf)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if pass, kind := filterFrame(frame); !pass {
			rl.rep.refuse(refusal{Reason: "dropped-frame", Detail: kind})
			continue
		}
		pkt := stack.NewPacketBuffer(stack.PacketBufferOptions{Payload: buffer.MakeWithData(frame)})
		rl.link.InjectInbound(header.Ethernet(frame).Type(), pkt)
		pkt.DecRef()
	}
}

func (rl *Relay) serveDNS(c *gonet.UDPConn) {
	buf := make([]byte, 1500)
	for {
		n, from, err := c.ReadFrom(buf)
		if err != nil {
			return
		}
		resp, res, err := answer(rl.names, buf[:n])
		if err != nil {
			continue
		}
		switch {
		case res.refused != "":
			rl.rep.refuseLocal("dns_refused", "cause", res.refused, "type", res.qtype)
		case res.evicted != "":
			rl.log.Info("evict", "name", res.evicted, "for", res.name)
		}
		_, _ = c.WriteTo(resp, from)
	}
}

func (rl *Relay) handleTCP(r *tcp.ForwarderRequest) {
	id := r.ID()
	port := id.LocalPort
	name, named := rl.names.lookup(id.LocalAddress.As4())
	if !named {
		// Not an address the relay handed out, so the flow has no name to put
		// to the proxy: reset at the SYN, nothing leaves the stack.
		r.Complete(true)
		rl.rep.refuse(refusal{Reason: "literal-ip", Host: id.LocalAddress.String(), Port: int(port)})
		return
	}
	flow := newFlowID()
	if port == 80 {
		gc := accept(r)
		if gc == nil {
			return
		}
		defer gc.Close()
		tr := rl.proxy.forwarder()
		defer tr.CloseIdleConnections()
		if reason := serveHTTP(gc, name, tr); reason != "" {
			rl.rep.refuse(refusal{Flow: flow, Reason: reason, Host: name, Port: 80})
		}
		return
	}
	t0 := time.Now()
	ok, status, err := rl.proxy.ask(name, port, flow)
	if !ok {
		// The proxy logged its own refusal or hold; the guest sees a reset.
		r.Complete(true)
		rl.log.Info("tcp_deny", "flow", flow, "host", name, "port", port, "proxy_status", status, "err", errString(err))
		return
	}
	gc := accept(r)
	if gc == nil {
		return
	}
	defer gc.Close()
	fl, err := readFlight(gc)
	var reason, detail string
	switch {
	case errors.Is(err, errSNIMissing):
		reason = "sni-missing"
	case err != nil:
		return // the guest went away; the parked ask expires with no row
	case fl.kind == flightTLS && fl.sni != name:
		reason, detail = "sni-mismatch", fl.sni
	case fl.kind == flightNone && port == 443:
		reason = "first-flight-timeout"
	}
	if reason != "" {
		rl.rep.refuse(refusal{Flow: flow, Reason: reason, Host: name, Port: int(port), Detail: detail})
		return
	}
	pc, status, err := rl.proxy.tunnel(name, port, flow)
	if pc == nil {
		rl.log.Info("tcp_deny", "flow", flow, "host", name, "port", port, "proxy_status", status, "err", errString(err))
		return
	}
	defer pc.Close()
	rl.log.Info("tcp_allow", "flow", flow, "host", name, "port", port, "connect_ms", time.Since(t0).Milliseconds())
	if len(fl.bytes) > 0 {
		if _, err := pc.Write(fl.bytes); err != nil {
			return
		}
	}
	splice(gc, pc)
}

// accept completes the guest's handshake.
func accept(r *tcp.ForwarderRequest) *gonet.TCPConn {
	var wq waiter.Queue
	ep, err := r.CreateEndpoint(&wq)
	if err != nil {
		r.Complete(true)
		return nil
	}
	r.Complete(false)
	return gonet.NewTCPConn(&wq, ep)
}

// splice copies both ways, half-closing each side when the other finishes.
func splice(gc *gonet.TCPConn, pc net.Conn) {
	done := make(chan struct{}, 2)
	go func() {
		_, _ = io.Copy(pc, gc)
		if cw, ok := pc.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
		done <- struct{}{}
	}()
	go func() {
		_, _ = io.Copy(gc, pc)
		_ = gc.CloseWrite()
		done <- struct{}{}
	}()
	<-done
	<-done
}

func newFlowID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// Listen serves QEMU's stream socket at path (mode 0660, for QEMU's group)
// until ctx ends, one connection at a time.
func (rl *Relay) Listen(ctx context.Context, path string) error {
	l, err := (&net.ListenConfig{}).Listen(ctx, "unix", path)
	if err != nil {
		return err
	}
	go func() { <-ctx.Done(); _ = l.Close() }()
	if err := os.Chmod(path, 0o660); err != nil {
		return err
	}
	rl.log.Info("listening", "socket", path, "proxy", rl.proxy.addr)
	for {
		c, err := l.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		rl.log.Info("qemu_connected")
		err = rl.Serve(c)
		_ = c.Close()
		rl.log.Info("qemu_disconnected", "err", errString(err))
	}
}
