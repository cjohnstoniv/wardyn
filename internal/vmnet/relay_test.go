// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bufio"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	"github.com/cjohnstoniv/wardyn/internal/vmnet/vmnettest"
)

// fakeProxy speaks the proxy's side of the relay contract: asks for allowed
// names park (204), the rest are refused (403); a tunnel echoes what it gets
// after sending banner; refusals and forward requests are recorded.
type fakeProxy struct {
	addr    string
	allowed map[string]bool
	banner  string
	mu      sync.Mutex
	events  []string
	refused []refusal
	tunnel  chan []byte
}

func (f *fakeProxy) note(ev string) {
	f.mu.Lock()
	f.events = append(f.events, ev)
	f.mu.Unlock()
}

func (f *fakeProxy) snapshot() ([]string, []refusal) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...), append([]refusal(nil), f.refused...)
}

func startFakeProxy(t *testing.T, allowed ...string) *fakeProxy {
	t.Helper()
	f := &fakeProxy{allowed: map[string]bool{}, tunnel: make(chan []byte, 8)}
	for _, a := range allowed {
		f.allowed[a] = true
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.addr = l.Addr().String()
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodConnect:
			stage, flow := r.Header.Get(stageHeader), r.Header.Get(flowHeader)
			f.note(stage + " " + r.Host + " " + flow)
			if !f.allowed[r.Host] {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			if stage == "ask" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			c, brw, _ := w.(http.Hijacker).Hijack()
			defer c.Close()
			_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n"+f.banner)
			_ = c.SetReadDeadline(time.Now().Add(time.Second))
			b, _ := io.ReadAll(brw)
			f.tunnel <- b
		case r.URL.Path == refusalsPath && r.URL.Host == "":
			var rf refusal
			_ = json.NewDecoder(r.Body).Decode(&rf)
			f.mu.Lock()
			f.refused = append(f.refused, rf)
			f.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			f.note("forward " + r.Method + " " + r.URL.String())
			_, _ = io.WriteString(w, "forwarded")
		}
	})}
	go func() { _ = srv.Serve(l) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f
}

func newTestRelay(t *testing.T, f *fakeProxy) (*Relay, *vmnettest.Guest) {
	t.Helper()
	rl, err := New(Config{Proxy: f.addr})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	return rl, vmnettest.Attach(t, rl.Serve)
}

// waitFor polls cond briefly: the relay reports and logs off the guest's path.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func (f *fakeProxy) count(reason string) int {
	_, rs := f.snapshot()
	n := 0
	for _, r := range rs {
		if r.Reason == reason {
			n++
		}
	}
	return n
}

func refusedWith(f *fakeProxy, reason string) func() bool {
	return func() bool { return f.count(reason) > 0 }
}

func TestRelayTLSFlows(t *testing.T) {
	defer func(h, w time.Duration) { helloBudget, firstWait = h, w }(helloBudget, firstWait)
	helloBudget, firstWait = 500*time.Millisecond, 500*time.Millisecond
	f := startFakeProxy(t, "origin.test:443", "origin.test:8443", "origin.test:2222")
	_, g := newTestRelay(t, f)
	addr := g.Resolve(t, "origin", "test")

	tlsDial := func(port uint16, sni string) {
		c, err := g.Dial(addr, port, 3*time.Second)
		if err != nil {
			t.Fatalf("dial :%d: %v", port, err)
		}
		defer c.Close()
		tc := tls.Client(c, &tls.Config{ServerName: sni, InsecureSkipVerify: true})
		_ = tc.SetDeadline(time.Now().Add(time.Second))
		_ = tc.Handshake()
	}

	t.Run("matching SNI tunnels with the ClientHello first", func(t *testing.T) {
		tlsDial(443, "origin.test")
		select {
		case b := <-f.tunnel:
			if len(b) < 5 || b[0] != 0x16 {
				t.Fatalf("tunnel got %d bytes not starting with a ClientHello", len(b))
			}
		case <-time.After(3 * time.Second):
			t.Fatal("no tunnel")
		}
		ev, _ := f.snapshot()
		if len(ev) != 2 || !strings.HasPrefix(ev[0], "ask origin.test:443 ") || !strings.HasPrefix(ev[1], "tunnel origin.test:443 ") ||
			strings.Fields(ev[0])[2] != strings.Fields(ev[1])[2] {
			t.Fatalf("events %q: want one ask then one tunnel with the same flow id", ev)
		}
	})
	t.Run("SNI mismatch is refused before any tunnel, on 443 and a granted port", func(t *testing.T) {
		for _, port := range []uint16{443, 8443} {
			before, _ := f.snapshot()
			n := f.count("sni-mismatch")
			tlsDial(port, "denied.test")
			// The second report of a kind within a second rides the next flush.
			waitFor(t, "sni-mismatch report", func() bool { return f.count("sni-mismatch") > n })
			ev, rs := f.snapshot()
			for _, e := range ev[len(before):] {
				if strings.HasPrefix(e, "tunnel") {
					t.Fatalf(":%d: %q after a mismatched SNI", port, e)
				}
			}
			last := rs[len(rs)-1]
			ask := strings.Fields(ev[len(ev)-1])
			if last.Detail != "denied.test" || last.Host != "origin.test" || last.Flow != ask[2] {
				t.Fatalf(":%d: report %+v, ask %q: the report must name the ask's flow", port, last, ask)
			}
		}
	})
	t.Run("delayed ClientHello is refused", func(t *testing.T) {
		c, err := g.Dial(addr, 443, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = c.Write([]byte{0x16, 3, 1})
		waitFor(t, "sni-missing report", refusedWith(f, "sni-missing"))
	})
	t.Run("nothing sent on 443 is refused", func(t *testing.T) {
		c, err := g.Dial(addr, 443, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		waitFor(t, "first-flight-timeout report", refusedWith(f, "first-flight-timeout"))
	})
	t.Run("a server-speaks-first protocol on a granted port tunnels after the wait", func(t *testing.T) {
		f.banner = "SSH-2.0-origin\r\n"
		defer func() { f.banner = "" }()
		c, err := g.Dial(addr, 2222, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil || line != "SSH-2.0-origin\r\n" {
			t.Fatalf("banner %q, %v", line, err)
		}
		_ = c.Close()
		<-f.tunnel
	})
}

func TestRelayRefusals(t *testing.T) {
	f := startFakeProxy(t, "origin.test:443")
	rl, g := newTestRelay(t, f)

	t.Run("a name the proxy refuses resets the SYN", func(t *testing.T) {
		addr := g.Resolve(t, "denied", "test")
		if c, err := g.Dial(addr, 443, 3*time.Second); err == nil {
			c.Close()
			t.Fatal("connected to a refused name")
		}
		ev, _ := f.snapshot()
		if len(ev) != 1 || !strings.HasPrefix(ev[0], "ask denied.test:443") {
			t.Fatalf("events %q", ev)
		}
	})
	t.Run("a literal address is reset at the SYN and reported", func(t *testing.T) {
		if c, err := g.Dial([4]byte{1, 1, 1, 1}, 443, 3*time.Second); err == nil {
			c.Close()
			t.Fatal("connected to a literal address")
		}
		waitFor(t, "literal-ip report", refusedWith(f, "literal-ip"))
	})
	t.Run("UDP other than DNS is dropped and reported", func(t *testing.T) {
		_ = g.SendUDP([4]byte{8, 8, 8, 8}, 53, []byte("q"))
		waitFor(t, "udp report", refusedWith(f, "udp"))
	})
	t.Run("ICMP and IPv6 frames are dropped and reported", func(t *testing.T) {
		_ = g.SendRaw(0x86dd, make([]byte, 40))
		_ = g.SendRaw(0x0800, ipv4Frame(1)[14:])
		waitFor(t, "ipv6 and icmp drops", func() bool {
			_, rs := f.snapshot()
			kinds := map[string]bool{}
			for _, r := range rs {
				if r.Reason == "dropped-frame" {
					kinds[r.Detail] = true
				}
			}
			return kinds["ipv6"] && kinds["icmp"]
		})
	})
	t.Run("a CR/LF label is REFUSED and never asked", func(t *testing.T) {
		m, err := g.Query([]string{"evil\r\nx", "test"}, dnsmessage.TypeA)
		if err != nil || m.Header.RCode != dnsmessage.RCodeRefused || len(m.Answers) != 0 {
			t.Fatalf("reply %+v %v", m, err)
		}
		if len(rl.names.byName) != 1 {
			t.Fatalf("table holds %d names, want only denied.test", len(rl.names.byName))
		}
	})
	t.Run("port 80 goes as a forward request, not an ask", func(t *testing.T) {
		addr := g.Resolve(t, "origin", "test")
		c, err := g.Dial(addr, 80, 3*time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		_, _ = io.WriteString(c, "GET /x HTTP/1.1\r\nHost: origin.test\r\nConnection: close\r\n\r\n")
		_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
		resp, err := http.ReadResponse(bufio.NewReader(c), nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("resp %v %v", resp, err)
		}
		ev, _ := f.snapshot()
		if last := ev[len(ev)-1]; last != "forward GET http://origin.test/x" {
			t.Fatalf("last event %q", last)
		}
	})
}

func TestRelayDropsOversizedFrames(t *testing.T) {
	f := startFakeProxy(t)
	rl, err := New(Config{Proxy: f.addr})
	if err != nil {
		t.Fatal(err)
	}
	defer rl.Close()
	a, b := net.Pipe()
	done := make(chan error, 1)
	go func() { done <- rl.Serve(a) }()
	go func() { _, _ = b.Write([]byte{0, 1, 0, 15}) }() // 65,551 bytes: one past the bound
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "out of bounds") {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("an oversized frame length did not end the connection")
	}
}
