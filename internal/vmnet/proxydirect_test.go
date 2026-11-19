// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/adapters/gonet"
	"gvisor.dev/gvisor/pkg/tcpip/network/ipv4"

	"github.com/cjohnstoniv/wardyn/internal/vmnet/vmnettest"
)

const guestProxyAddr = "wardyn-proxy:3128"

// rawProxy stands in for the agent listener the relay's splice opens: it
// records each accepted connection's bytes (one entry per connection) and
// echoes them, closing on EOF.
type rawProxy struct {
	addr  string
	mu    sync.Mutex
	conns []*bytes.Buffer
}

func startRawProxy(t *testing.T) *rawProxy {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	rp := &rawProxy{addr: l.Addr().String()}
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			buf := new(bytes.Buffer)
			rp.mu.Lock()
			rp.conns = append(rp.conns, buf)
			rp.mu.Unlock()
			go func() {
				defer c.Close()
				b := make([]byte, 4096)
				for {
					n, err := c.Read(b)
					if n > 0 {
						rp.mu.Lock()
						buf.Write(b[:n])
						rp.mu.Unlock()
						if _, werr := c.Write(b[:n]); werr != nil {
							return
						}
					}
					if err != nil {
						return
					}
				}
			}()
		}
	}()
	t.Cleanup(func() { _ = l.Close() })
	return rp
}

// seen returns what each accepted connection has received so far.
func (rp *rawProxy) seen() []string {
	rp.mu.Lock()
	defer rp.mu.Unlock()
	out := make([]string, len(rp.conns))
	for i, b := range rp.conns {
		out[i] = b.String()
	}
	return out
}

// directDial sends the guest-facing proxy address to the guest-side fake and
// any other address where it was given.
func directDial(guestSide string, calls *atomic.Int64) func(context.Context, string, string) (net.Conn, error) {
	return func(ctx context.Context, network, addr string) (net.Conn, error) {
		if calls != nil {
			calls.Add(1)
		}
		if addr == guestProxyAddr {
			addr = guestSide
		}
		return (&net.Dialer{Resolver: &net.Resolver{}}).DialContext(ctx, network, addr)
	}
}

// directRelay is a relay whose splice target is guestSide and whose own
// requests go to f; the guest has resolved the proxy's name.
func directRelay(t *testing.T, f *fakeProxy, guestSide string, maxOpaque int, logs *lockedBuf) (*Relay, *vmnettest.Guest, [4]byte) {
	t.Helper()
	rl, err := New(Config{
		Proxy: f.addr, GuestProxy: guestProxyAddr, Dial: directDial(guestSide, nil),
		Log: slog.New(slog.NewJSONHandler(logs, nil)), maxOpaque: maxOpaque,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	g := vmnettest.Attach(t, rl.Serve)
	return rl, g, g.Resolve(t, "wardyn-proxy")
}

func TestProxyDirectCarriesBytesUnchanged(t *testing.T) {
	f := startFakeProxy(t)
	raw := startRawProxy(t)
	var logs lockedBuf
	_, g, addr := directRelay(t, f, raw.addr, 0, &logs)

	payload := []byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\nWardyn-Vmnet-Flow: 0011223344556677\r\nWardyn-Vmnet-Stage: ask\r\n\r\n")
	payload = append(payload, 0x00, 0xff, 0x16, 0x03, 0x01, 0x80)
	c, err := g.Dial(addr, 3128, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write(payload); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	got := make([]byte, len(payload))
	if _, err := io.ReadFull(c, got); err != nil {
		t.Fatalf("echo: %v", err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("echoed %q, want %q", got, payload)
	}
	if seen := raw.seen(); len(seen) != 1 || seen[0] != string(payload) {
		t.Fatalf("the proxy's agent port saw %q, want one connection with exactly the payload", seen)
	}
	if evs, rs := f.snapshot(); len(evs) != 0 || len(rs) != 0 {
		t.Fatalf("the relay listener saw events %v, reports %v: nothing should reach it", evs, rs)
	}
	waitFor(t, "proxy-direct log line", func() bool { return strings.Contains(logs.String(), `"msg":"proxy-direct"`) })
	if strings.Contains(logs.String(), "example.com") {
		t.Fatalf("the log carries request content: %s", logs.String())
	}
}

func TestProxyDirectExplicitHTTPS(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "hello")
	}))
	defer origin.Close()

	var mu sync.Mutex
	var connects []string
	explicit := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		connects = append(connects, r.Host+"|"+r.Header.Get(flowHeader)+"|"+r.Header.Get(stageHeader))
		mu.Unlock()
		up, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		defer up.Close()
		c, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer c.Close()
		_, _ = io.WriteString(c, "HTTP/1.1 200 Connection Established\r\n\r\n")
		done := make(chan struct{})
		go func() { _, _ = io.Copy(up, c); close(done) }()
		_, _ = io.Copy(c, up)
		<-done
	}))
	defer explicit.Close()

	f := startFakeProxy(t)
	var logs lockedBuf
	_, g, addr := directRelay(t, f, strings.TrimPrefix(explicit.URL, "http://"), 0, &logs)

	pool := x509.NewCertPool()
	pool.AddCert(origin.Certificate())
	proxyURL, _ := url.Parse("http://" + guestProxyAddr)
	tr := &http.Transport{
		Proxy: http.ProxyURL(proxyURL),
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			c, err := g.Dial(addr, 3128, 3*time.Second)
			if err != nil {
				return nil, err
			}
			return c, nil
		},
		TLSClientConfig: &tls.Config{RootCAs: pool},
	}
	defer tr.CloseIdleConnections()
	resp, err := (&http.Client{Transport: tr, Timeout: 10 * time.Second}).Get("https://example.com/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if string(body) != "hello" {
		t.Fatalf("body %q, want hello", body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(connects) != 1 || connects[0] != "example.com:443||" {
		t.Fatalf("explicit proxy saw %q, want one CONNECT for example.com:443 with empty flow and stage headers", connects)
	}
}

func TestProxyDirectOnlyExactNameAndPort(t *testing.T) {
	f := startFakeProxy(t)
	raw := startRawProxy(t)
	var logs lockedBuf
	_, g, addr := directRelay(t, f, raw.addr, 0, &logs)
	other := g.Resolve(t, "origin", "test")

	if c, err := g.Dial(addr, 443, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("wardyn-proxy:443 connected")
	}
	if c, err := g.Dial(other, 3128, 2*time.Second); err == nil {
		c.Close()
		t.Fatal("origin.test:3128 connected")
	}
	waitFor(t, "both asks", func() bool {
		evs, _ := f.snapshot()
		var a, b bool
		for _, e := range evs {
			a = a || strings.HasPrefix(e, "ask wardyn-proxy:443 ")
			b = b || strings.HasPrefix(e, "ask origin.test:3128 ")
		}
		return a && b
	})
	if seen := raw.seen(); len(seen) != 0 {
		t.Fatalf("the splice target saw %d connections, want 0", len(seen))
	}
	if strings.Contains(logs.String(), "proxy-direct") {
		t.Fatalf("a proxy-direct line for a flow that is not the guest's proxy: %s", logs.String())
	}
}

func TestProxyDirectPrepStillResets(t *testing.T) {
	var logs lockedBuf
	var dials atomic.Int64
	rl, err := New(Config{
		Prep: true, Proxy: "wardyn-proxy:3130", GuestProxy: guestProxyAddr,
		Dial: directDial("127.0.0.1:1", &dials), Log: slog.New(slog.NewJSONHandler(&logs, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(rl.Close)
	gs := stackGuest(t, rl.Serve)

	a, _, _ := newNameTable(time.Now, maxNames).resolve("wardyn-proxy")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	c, err := gonet.DialContextTCP(ctx, gs, tcpip.FullAddress{NIC: 1, Addr: tcpip.AddrFrom4(a), Port: 3128}, ipv4.ProtocolNumber)
	cancel()
	if err == nil {
		c.Close()
		t.Fatal("guest connected to the proxy's name through a preparation relay")
	}
	waitFor(t, "prep-egress refusal logged", func() bool { return strings.Contains(logs.String(), `"reason":"prep-egress"`) })
	if n := dials.Load(); n != 0 {
		t.Fatalf("%d dials from a preparation relay", n)
	}
	if strings.Contains(logs.String(), "proxy-direct") {
		t.Fatalf("a proxy-direct line in a preparation relay: %s", logs.String())
	}
}

func TestProxyDirectAtTheTunnelLimit(t *testing.T) {
	f := startFakeProxy(t)
	raw := startRawProxy(t)
	var logs lockedBuf
	rl, g, addr := directRelay(t, f, raw.addr, 1, &logs)

	first, err := g.Dial(addr, 3128, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := first.Write([]byte{'x'}); err != nil {
		t.Fatal(err)
	}
	_ = first.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(first, make([]byte, 1)); err != nil {
		t.Fatalf("first flow echo: %v", err)
	}
	waitFor(t, "the first flow's slot", opaqueIs(rl, 1))

	if c, err := g.Dial(addr, 3128, 3*time.Second); err == nil {
		c.Close()
		t.Fatal("a second proxy-direct flow past the bound was accepted")
	}
	waitFor(t, "tunnel-limit report", refusedWith(f, reasonTunnelLimit))
	if rl.opaque.Load() != 1 {
		t.Fatalf("held %d, want 1", rl.opaque.Load())
	}
	if n := len(raw.seen()); n != 1 {
		t.Fatalf("the splice target saw %d connections, want 1", n)
	}
	first.Close()
	waitFor(t, "the slot to free", opaqueIs(rl, 0))
}
