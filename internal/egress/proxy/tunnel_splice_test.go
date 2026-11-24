// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// tunHello is a client's opening flight shaped as a TLS handshake record, so the first-bytes guard is armed;
// its last byte tells tunUpstream what to do after answering it.
func tunHello(mode byte) []byte {
	return []byte{0x16, 0x03, 0x01, 0x00, 0x05, 'h', 'e', 'l', 'l', mode}
}

// tunAnswer is the upstream's first record: a TLS handshake record longer than an alert, which the guard passes.
var tunAnswer = append([]byte{0x16, 0x03, 0x03, 0x00, 0x10}, bytes.Repeat([]byte{'s'}, 16)...)

// tunUpstream is a loopback TCP upstream. Each connection reads a tunHello and answers tunAnswer, then by the
// hello's mode echoes ('e'), sends tunBenchBytes and closes ('d'), or reads to the end and reports the count on
// got ('u'). Any other mode, or no hello, gets plain HTTP instead of TLS.
func tunUpstream(tb testing.TB, got chan<- int64) string {
	tb.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				hello := make([]byte, len(tunHello(0)))
				if _, err := io.ReadFull(c, hello); err != nil {
					return
				}
				mode := hello[len(hello)-1]
				if mode != 'e' && mode != 'd' && mode != 'u' {
					_, _ = io.WriteString(c, "HTTP/1.1 403 Forbidden\r\nContent-Length: 0\r\n\r\n")
					return
				}
				if _, err := c.Write(tunAnswer); err != nil {
					return
				}
				switch mode {
				case 'e':
					_, _ = io.Copy(c, c)
				case 'd':
					chunk := make([]byte, 1<<20)
					for sent := 0; sent < tunBenchBytes; sent += len(chunk) {
						if _, err := c.Write(chunk); err != nil {
							return
						}
					}
				case 'u':
					n, _ := io.Copy(io.Discard, c)
					got <- n
				}
			}()
		}
	}()
	return ln.Addr().String()
}

// tunProxy serves a proxy that allows allowed.test and dials it at upstream, behind the listener wrapper the
// sidecar's own listener adds (trackedListener), so its tunnels wear every wrapper a sandbox's do.
func tunProxy(tb testing.TB, upstream string) (*Proxy, string) {
	tb.Helper()
	p := newProxy(Options{
		RunID:    uuid.New(),
		Policy:   CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"allowed.test"}}),
		Sink:     &decisionSink{out: io.Discard, ch: make(chan egress.DecisionLog, 64)},
		Resolver: publicResolver{},
		Dial:     redirectDial(upstream),
	})
	srv := httptest.NewUnstartedServer(p)
	srv.Listener = &trackedListener{Listener: srv.Listener, set: &liveState{}}
	srv.Start()
	tb.Cleanup(srv.Close)
	return p, srv.Listener.Addr().String()
}

// tunOpen opens a CONNECT tunnel to allowed.test through the proxy at addr, sends tunHello(mode) and reads the
// upstream's answer. The reader serves the rest of the tunnel.
func tunOpen(tb testing.TB, addr string, mode byte) (net.Conn, *bufio.Reader) {
	tb.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, "CONNECT allowed.test:443 HTTP/1.1\r\nHost: allowed.test:443\r\n\r\n"); err != nil {
		tb.Fatal(err)
	}
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil || resp.StatusCode != http.StatusOK {
		tb.Fatalf("CONNECT: %v %v", resp, err)
	}
	if _, err := c.Write(tunHello(mode)); err != nil {
		tb.Fatal(err)
	}
	if mode == 'e' || mode == 'd' || mode == 'u' {
		got := make([]byte, len(tunAnswer))
		if _, err := io.ReadFull(br, got); err != nil || !bytes.Equal(got, tunAnswer) {
			tb.Fatalf("the upstream's answer: %q %v", got, err)
		}
	}
	return c, br
}

// tunCountSplices counts the bytes tunnel copies socket to socket for the rest of the test.
func tunCountSplices(t *testing.T) *atomic.Int64 {
	t.Helper()
	var n atomic.Int64
	saved := tunnelReadFrom
	tunnelReadFrom = func(dst *net.TCPConn, src io.Reader) (int64, error) {
		if _, ok := src.(*net.TCPConn); !ok {
			t.Errorf("a spliced copy read from %T, want the socket itself", src)
		}
		moved, err := saved(dst, src)
		n.Add(moved)
		return moved, err
	}
	t.Cleanup(func() { tunnelReadFrom = saved })
	return &n
}

// tunWaitClosed waits until every tunnel of p has ended.
func tunWaitClosed(t *testing.T, p *Proxy) {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); p.openTunnels.Load() != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the tunnel never ended")
		}
	}
}

// TestTunnelSplicesOnceTheGuardSettles: an opaque CONNECT tunnel, worn as a sandbox's is, relays its first
// flights through the wrappers, so the first-bytes guard judges them, and then copies socket to socket. 8 MiB each
// way arrive whole, nearly all of them by the sockets' ReadFrom, also when that copy comes back every
// millisecond, and the bytes it moves still mark the run active.
func TestTunnelSplicesOnceTheGuardSettles(t *testing.T) {
	for _, tick := range []time.Duration{tunnelSpliceTick, time.Millisecond} {
		t.Run(tick.String(), func(t *testing.T) {
			saved := tunnelSpliceTick
			tunnelSpliceTick = tick
			t.Cleanup(func() { tunnelSpliceTick = saved })
			spliced := tunCountSplices(t)
			p, addr := tunProxy(t, tunUpstream(t, nil))
			c, br := tunOpen(t, addr, 'e')
			if n := spliced.Load(); n != 0 {
				t.Fatalf("%d bytes were spliced before the upstream's first record was judged", n)
			}

			const size = 8 << 20
			out := make([]byte, size)
			_, _ = rand.Read(out)
			go func() { _, _ = c.Write(out) }()
			back := make([]byte, size)
			if _, err := io.ReadFull(br, back); err != nil {
				t.Fatalf("echo: %v", err)
			}
			if sha256.Sum256(back) != sha256.Sum256(out) {
				t.Fatal("the bytes that came back differ from the bytes sent")
			}
			if tick == time.Millisecond {
				p.streamMoved.Store(false)
				if _, err := c.Write([]byte("ping")); err != nil {
					t.Fatal(err)
				}
				if _, err := io.ReadFull(br, back[:4]); err != nil {
					t.Fatal(err)
				}
				for deadline := time.Now().Add(5 * time.Second); !p.streamMoved.Load(); time.Sleep(time.Millisecond) {
					if time.Now().After(deadline) {
						t.Fatal("bytes moved socket to socket did not mark the run active")
					}
				}
			}
			_ = c.Close()
			tunWaitClosed(t, p)
			if n := spliced.Load(); n < 2*size-1<<20 {
				t.Fatalf("%d of %d bytes went socket to socket, want all but the first flights", n, 2*size)
			}
		})
	}
}

// TestTunnelGuardRefusesBeforeAnySplice: an upstream that answers a TLS hello in plain HTTP is refused by the
// first-bytes guard, as before: the client gets none of it, and nothing was copied socket to socket.
func TestTunnelGuardRefusesBeforeAnySplice(t *testing.T) {
	spliced := tunCountSplices(t)
	p, addr := tunProxy(t, tunUpstream(t, nil))
	c, br := tunOpen(t, addr, 'x')
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	if got, err := io.ReadAll(br); len(got) != 0 {
		t.Fatalf("the client got %q (%v) from an upstream that answered a TLS hello in plaintext", got, err)
	}
	tunWaitClosed(t, p)
	if n := spliced.Load(); n != 0 {
		t.Fatalf("%d bytes were spliced on a tunnel the guard refused", n)
	}
}

// tunBenchBytes is what each BenchmarkProxyTunnelThroughput iteration moves.
const tunBenchBytes = 1 << 30

// BenchmarkProxyTunnelThroughput moves 1 GiB through one opaque CONNECT tunnel on loopback, worn as a sandbox's is:
// download (upstream to client) and upload (client to upstream). Run with -benchtime 1x; MB/s is the figure.
func BenchmarkProxyTunnelThroughput(b *testing.B) {
	got := make(chan int64, 1)
	_, addr := tunProxy(b, tunUpstream(b, got))
	b.Run("download", func(b *testing.B) {
		b.SetBytes(tunBenchBytes)
		for range b.N {
			_, br := tunOpen(b, addr, 'd')
			if n, err := io.Copy(io.Discard, br); n != tunBenchBytes {
				b.Fatalf("downloaded %d bytes: %v", n, err)
			}
		}
	})
	b.Run("upload", func(b *testing.B) {
		b.SetBytes(tunBenchBytes)
		chunk := make([]byte, 1<<20)
		for range b.N {
			c, _ := tunOpen(b, addr, 'u')
			for sent := 0; sent < tunBenchBytes; sent += len(chunk) {
				if _, err := c.Write(chunk); err != nil {
					b.Fatal(err)
				}
			}
			_ = c.(*net.TCPConn).CloseWrite()
			if n := <-got; n != tunBenchBytes {
				b.Fatalf("uploaded %d bytes", n)
			}
		}
	})
}
