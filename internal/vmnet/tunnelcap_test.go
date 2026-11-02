// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/stack"

	"github.com/cjohnstoniv/wardyn/internal/vmnet/vmnettest"
)

// holdTunnel is one tunnel the proxy holds open; reset makes its end a RST.
type holdTunnel struct {
	release chan struct{}
	reset   bool
	ended   bool
}

// holdProxy allows every name but those in denied, answers an ask once gate (if
// set) is closed, and holds each tunnel open until the test closes it.
type holdProxy struct {
	addr    string
	mu      sync.Mutex
	denied  map[string]bool
	gate    chan struct{}
	asks    int
	tunnels []*holdTunnel
	refused []refusal
}

func startHoldProxy(t *testing.T) *holdProxy {
	t.Helper()
	h := &holdProxy{denied: map[string]bool{}}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	h.addr = l.Addr().String()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodConnect {
			var rf refusal
			_ = json.NewDecoder(r.Body).Decode(&rf)
			h.mu.Lock()
			h.refused = append(h.refused, rf)
			h.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Header.Get(stageHeader) == "ask" {
			h.mu.Lock()
			h.asks++
			gate, deny := h.gate, h.denied[r.Host]
			h.mu.Unlock()
			if gate != nil {
				<-gate
			}
			if deny {
				w.WriteHeader(http.StatusForbidden)
			} else {
				w.WriteHeader(http.StatusNoContent)
			}
			return
		}
		c, _, _ := w.(http.Hijacker).Hijack()
		defer c.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		ht := &holdTunnel{release: make(chan struct{})}
		h.mu.Lock()
		h.tunnels = append(h.tunnels, ht)
		h.mu.Unlock()
		<-ht.release
		h.mu.Lock()
		reset := ht.reset
		h.mu.Unlock()
		if tc, ok := c.(*net.TCPConn); ok && reset {
			_ = tc.SetLinger(0)
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() {
		h.closeAll()
		_ = srv.Close()
	})
	return h
}

func (h *holdProxy) snapshot() (asks, tunnels int, refused []refusal) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asks, len(h.tunnels), append([]refusal(nil), h.refused...)
}

// closeTunnel ends the proxy's side of the nth tunnel opened.
func (h *holdProxy) closeTunnel(n int) { h.endTunnel(n, false) }

// resetTunnel ends the proxy's side of the nth tunnel with a RST.
func (h *holdProxy) resetTunnel(n int) { h.endTunnel(n, true) }

func (h *holdProxy) endTunnel(n int, reset bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if ht := h.tunnels[n]; !ht.ended {
		ht.ended, ht.reset = true, reset
		close(ht.release)
	}
}

func (h *holdProxy) closeAll() {
	for n := 0; n < h.tunnelCount(); n++ {
		h.closeTunnel(n)
	}
}

func (h *holdProxy) tunnelCount() int {
	_, n, _ := h.snapshot()
	return n
}

// capRelay is a relay whose bound is n, with the first-byte wait cut short so a
// silent guest tunnels at once.
func capRelay(t *testing.T, h *holdProxy, n int) (*Relay, *vmnettest.Guest, [4]byte) {
	t.Helper()
	old := firstWaitOther
	t.Cleanup(func() { firstWaitOther = old })
	firstWaitOther = 50 * time.Millisecond
	rl, err := New(Config{Proxy: h.addr, maxOpaque: n})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	g := vmnettest.Attach(t, rl.Serve)
	return rl, g, g.Resolve(t, "origin", "test")
}

func opaqueIs(rl *Relay, want int64) func() bool {
	return func() bool { return rl.opaque.Load() == want }
}

func TestOpaqueTunnelsStopAtTheBound(t *testing.T) {
	h := startHoldProxy(t)
	const bound = 3
	rl, g, addr := capRelay(t, h, bound)

	var conns []net.Conn
	for i := 0; i < bound; i++ {
		c, err := g.Dial(addr, uint16(2000+i), 3*time.Second)
		if err != nil {
			t.Fatalf("tunnel %d: %v", i, err)
		}
		defer c.Close()
		conns = append(conns, c)
		// One at a time, so the proxy's nth tunnel is the guest's nth connection.
		waitFor(t, "the tunnel", func() bool { return h.tunnelCount() == i+1 })
	}

	if c, err := g.Dial(addr, 2100, 3*time.Second); err == nil {
		c.Close()
		t.Fatal("a tunnel past the bound was accepted")
	}
	waitFor(t, "tunnel-limit report", func() bool { _, _, rs := h.snapshot(); return len(rs) == 1 })
	asks, tunnels, rs := h.snapshot()
	if rs[0].Reason != reasonTunnelLimit || rs[0].Host != "origin.test" || rs[0].Port != 2100 {
		t.Fatalf("report %+v, want %s for origin.test:2100", rs[0], reasonTunnelLimit)
	}
	if asks != bound || tunnels != bound || rl.opaque.Load() != bound {
		t.Fatalf("asks %d, tunnels %d, held %d: the refused flow must not be asked and the open ones must stay", asks, tunnels, rl.opaque.Load())
	}

	// Both directions of one tunnel end: its slot goes to the next flow.
	h.closeTunnel(0)
	_ = conns[0].Close()
	waitFor(t, "a freed slot", opaqueIs(rl, bound-1))
	c, err := g.Dial(addr, 2101, 3*time.Second)
	if err != nil {
		t.Fatalf("a flow after a slot freed: %v", err)
	}
	defer c.Close()
	waitFor(t, "the admitted tunnel", func() bool { return h.tunnelCount() == bound+1 })
}

func TestOpaquePendingAskHoldsASlot(t *testing.T) {
	h := startHoldProxy(t)
	h.gate = make(chan struct{})
	rl, g, addr := capRelay(t, h, 1)

	first := make(chan error, 1)
	go func() {
		c, err := g.Dial(addr, 2000, 10*time.Second)
		if c != nil {
			defer c.Close()
			waitFor(t, "the tunnel", func() bool { return h.tunnelCount() == 1 })
		}
		first <- err
	}()
	waitFor(t, "the pending ask", opaqueIs(rl, 1))
	if c, err := g.Dial(addr, 2001, 3*time.Second); err == nil {
		c.Close()
		t.Fatal("a flow was admitted beside a pending ask at the bound")
	}
	waitFor(t, "tunnel-limit report", func() bool { _, _, rs := h.snapshot(); return len(rs) == 1 && rs[0].Reason == reasonTunnelLimit })
	close(h.gate)
	if err := <-first; err != nil {
		t.Fatalf("the pending flow: %v", err)
	}
}

func TestOpaqueRefusedAskReturnsItsSlot(t *testing.T) {
	h := startHoldProxy(t)
	h.denied["denied.test:2000"] = true
	rl, g, addr := capRelay(t, h, 2)

	c, err := g.Dial(addr, 2001, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "the first tunnel", func() bool { return h.tunnelCount() == 1 })
	before := rl.opaque.Load()

	if c, err := g.Dial(g.Resolve(t, "denied", "test"), 2000, 3*time.Second); err == nil {
		c.Close()
		t.Fatal("connected to a refused name")
	}
	waitFor(t, "the slot back", opaqueIs(rl, before))
	time.Sleep(100 * time.Millisecond)
	if got := rl.opaque.Load(); got != before {
		t.Fatalf("counter %d after a refused ask, was %d", got, before)
	}

	// Exactly one slot was free: the next flow fits, the one after does not.
	c2, err := g.Dial(addr, 2002, 3*time.Second)
	if err != nil {
		t.Fatalf("the free slot: %v", err)
	}
	defer c2.Close()
	waitFor(t, "the second tunnel", func() bool { return h.tunnelCount() == 2 })
	if c3, err := g.Dial(addr, 2003, 3*time.Second); err == nil {
		c3.Close()
		t.Fatal("a third tunnel was admitted at a bound of two")
	}
}

func TestOpaqueSlotHeldUntilBothDirectionsEnd(t *testing.T) {
	h := startHoldProxy(t)
	rl, g, addr := capRelay(t, h, 1)

	c, err := g.Dial(addr, 2000, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "the tunnel", func() bool { return h.tunnelCount() == 1 })

	_ = c.CloseWrite() // the guest's direction ends; the proxy's is still open
	time.Sleep(300 * time.Millisecond)
	if got := rl.opaque.Load(); got != 1 {
		t.Fatalf("counter %d after a half-close, want 1", got)
	}
	if c2, err := g.Dial(addr, 2001, 3*time.Second); err == nil {
		c2.Close()
		t.Fatal("a half-closed tunnel's slot was given away")
	}

	h.closeTunnel(0)
	waitFor(t, "the slot after both ended", opaqueIs(rl, 0))
}

// The guest leg is a gVisor endpoint the relay accepted; read the options back
// from the relay's own stack.
func TestGuestLegHasKeepalive(t *testing.T) {
	h := startHoldProxy(t)
	rl, g, addr := capRelay(t, h, 1)
	c, err := g.Dial(addr, 2000, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	waitFor(t, "the tunnel", func() bool { return h.tunnelCount() == 1 })

	for _, te := range rl.stack.RegisteredEndpoints() {
		if keepaliveReads(te) {
			return
		}
	}
	t.Fatal("no endpoint on the relay's stack has keepalive on with the relay's timing")
}

func keepaliveReads(te stack.TransportEndpoint) bool {
	ep, ok := te.(tcpip.Endpoint)
	if !ok || !ep.SocketOptions().GetKeepAlive() {
		return false
	}
	var idle tcpip.KeepaliveIdleOption
	var interval tcpip.KeepaliveIntervalOption
	if ep.GetSockOpt(&idle) != nil || ep.GetSockOpt(&interval) != nil {
		return false
	}
	n, err := ep.GetSockOptInt(tcpip.KeepaliveCountOption)
	return err == nil && time.Duration(idle) == keepaliveIdle && time.Duration(interval) == keepaliveInterval && n == keepaliveCount
}

// A copy that ends in an error ends the whole tunnel: the other end may sit
// idle with its own half open, and the slot must not wait on it.
func TestOpaqueSlotFreedWhenACopyFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		fail func(h *holdProxy, g *vmnettest.Guest, c *gonet.TCPConn)
	}{
		{"the proxy side resets while the guest is idle", func(h *holdProxy, _ *vmnettest.Guest, _ *gonet.TCPConn) { h.resetTunnel(0) }},
		{"the guest resets while the proxy is idle", func(_ *holdProxy, g *vmnettest.Guest, c *gonet.TCPConn) { g.Abort(c) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := startHoldProxy(t)
			rl, g, addr := capRelay(t, h, 1)
			c, err := g.Dial(addr, 2000, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer c.Close()
			waitFor(t, "the tunnel", func() bool { return h.tunnelCount() == 1 })

			tc.fail(h, g, c)
			waitFor(t, "the slot after a failed copy", opaqueIs(rl, 0))

			c2, err := g.Dial(addr, 2001, 3*time.Second)
			if err != nil {
				t.Fatalf("a flow after the slot freed: %v", err)
			}
			defer c2.Close()
			waitFor(t, "the next tunnel", func() bool { return h.tunnelCount() == 2 })
		})
	}
}

func TestInFlightBoundLeavesRoomForTheTypedRefusal(t *testing.T) {
	if inFlightBound(maxOpaqueTunnels) <= maxOpaqueTunnels {
		t.Fatalf("production in-flight bound %d is not above the opaque cap %d", inFlightBound(maxOpaqueTunnels), maxOpaqueTunnels)
	}
	h := startHoldProxy(t)
	h.gate = make(chan struct{})
	const bound = 2
	rl, g, addr := capRelay(t, h, bound)

	for i := 0; i < bound; i++ {
		go func(port uint16) {
			if c, err := g.Dial(addr, port, 10*time.Second); err == nil {
				defer c.Close()
				waitFor(t, "the tunnel", func() bool { return h.tunnelCount() == bound })
			}
		}(uint16(2000 + i))
	}
	waitFor(t, "every slot pending", func() bool { a, _, _ := h.snapshot(); return a == bound && rl.opaque.Load() == bound })

	start := time.Now()
	if c, err := g.Dial(addr, 2100, 5*time.Second); err == nil {
		c.Close()
		t.Fatal("an opaque flow past the bound was admitted")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("the excess SYN was dropped, not reset")
	}
	waitFor(t, "tunnel-limit report", func() bool {
		_, _, rs := h.snapshot()
		return len(rs) == 1 && rs[0].Reason == reasonTunnelLimit && rs[0].Port == 2100
	})
	if asks, _, _ := h.snapshot(); asks != bound || rl.opaque.Load() != bound {
		t.Fatalf("asks %d, held %d: the refused flow admitted something extra", asks, rl.opaque.Load())
	}
	close(h.gate)
	waitFor(t, "the pending flows tunnel", func() bool { return h.tunnelCount() == bound })
}
