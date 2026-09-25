// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// countingUpstream407 is a loopback CONNECT proxy that always answers 407 —
// the corp-proxy-requires-auth case (T-53). accepts counts TCP accepts so the
// test can pin that a refused CONNECT triggers exactly one upstream
// connection, never a retry storm.
type countingUpstream407 struct {
	ln      net.Listener
	accepts int32
}

func startCountingUpstream407(t *testing.T) *countingUpstream407 {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	u := &countingUpstream407{ln: ln}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			atomic.AddInt32(&u.accepts, 1)
			go u.serve(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return u
}

func (u *countingUpstream407) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	if _, err := http.ReadRequest(br); err != nil {
		return
	}
	_, _ = c.Write([]byte("HTTP/1.1 407 Proxy Authentication Required\r\n" +
		"Proxy-Authenticate: Basic realm=\"corp\"\r\n" +
		"Content-Length: 0\r\n\r\n"))
}

// TestUpstreamCONNECT407DeniesOnceWithNamedCause pins T-53's first behaviour:
// a corp proxy answering 407 to the CONNECT becomes one builtin:dial-failed
// DENY naming the upstream leg and the 407, a 502 to the agent, and exactly
// one upstream connection (no retry storm).
func TestUpstreamCONNECT407DeniesOnceWithNamedCause(t *testing.T) {
	up407 := startCountingUpstream407(t)

	upCfg, err := parseUpstreamProxy("http://user:hunter2pass@" + up407.ln.Addr().String())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newUpstreamProxy(t, upCfg)
	buf := p.sink.out.(*bytes.Buffer)

	proxySrv := httptest.NewServer(p)
	conn, resp := connectThrough(t, proxySrv.URL, "tls.test:443")
	_ = conn.Close()
	// Close the proxy server (waits for the in-flight handler to return)
	// before reading the accept counter — no sleep-based settle.
	proxySrv.Close()

	if !strings.Contains(resp, "502") {
		t.Errorf("response = %q, want a 502 status line", resp)
	}

	d := findDecision(t, buf, "builtin:dial-failed")
	if d.Via != viaUpstreamProxy {
		t.Errorf("via = %q, want %q", d.Via, viaUpstreamProxy)
	}
	if !strings.HasPrefix(d.Cause, "upstream proxy connect: ") {
		t.Errorf("cause = %q, want an %q-prefixed stage", d.Cause, "upstream proxy connect: ")
	}
	if !strings.Contains(d.Cause, "407") {
		t.Errorf("cause = %q, want it to name the 407", d.Cause)
	}
	if strings.Contains(d.Cause, "hunter2pass") {
		t.Errorf("cause = %q leaked the upstream credential", d.Cause)
	}

	if got := atomic.LoadInt32(&up407.accepts); got != 1 {
		t.Errorf("upstream accepts = %d, want exactly 1 (no retry storm)", got)
	}
}

// firstWritePrefixed prefixes ONLY the first Write to the underlying conn —
// simulating a corp proxy that writes its CONNECT 200 and the first TLS
// handshake bytes in a single underlying Write (Nagle/cork coalescing a
// pipelined reply), the case prefixConn exists to survive.
type firstWritePrefixed struct {
	net.Conn
	prefix []byte
	wrote  bool
}

func (c *firstWritePrefixed) Write(b []byte) (int, error) {
	if c.wrote {
		return c.Conn.Write(b)
	}
	c.wrote = true
	if _, err := c.Conn.Write(append(append([]byte(nil), c.prefix...), b...)); err != nil {
		return 0, err
	}
	return len(b), nil
}

// TestUpstreamPipelinedTLSBytesReplayedByPrefixConn pins T-53's second
// behaviour: when the corp proxy's 200 and the first TLS bytes arrive in one
// Write, dialThroughUpstream's over-buffered read must come back as a
// *prefixConn whose replayed prefix lets the TLS handshake still complete.
func TestUpstreamPipelinedTLSBytesReplayedByPrefixConn(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	errCh := make(chan error, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			errCh <- fmt.Errorf("accept: %w", err)
			return
		}
		defer c.Close()
		if err := c.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
			errCh <- fmt.Errorf("set upstream deadline: %w", err)
			return
		}
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			errCh <- fmt.Errorf("read CONNECT: %w", err)
			return
		}
		wrapped := &firstWritePrefixed{Conn: c, prefix: []byte("HTTP/1.1 200 Connection Established\r\n\r\n")}
		tlsConn := tls.Client(wrapped, &tls.Config{InsecureSkipVerify: true})
		if err := tlsConn.Handshake(); err != nil {
			errCh <- fmt.Errorf("upstream tls handshake: %w", err)
			return
		}
		if _, err := tlsConn.Write([]byte("pong")); err != nil {
			errCh <- fmt.Errorf("write pong: %w", err)
			return
		}
		errCh <- nil
	}()

	up, err := parseUpstreamProxy("http://" + ln.Addr().String())
	if err != nil {
		t.Fatalf("parse upstream: %v", err)
	}
	p := newUpstreamProxy(t, up)

	conn, err := p.dialThroughUpstream(context.Background(), "tls.test", 443)
	if err != nil {
		t.Fatalf("dialThroughUpstream: %v", err)
	}
	defer conn.Close()

	pc, ok := conn.(*prefixConn)
	if !ok {
		t.Fatalf("conn = %T, want *prefixConn", conn)
	}
	if len(pc.prefix) == 0 {
		t.Fatalf("prefixConn has an empty prefix; the pipelined ClientHello bytes were not captured")
	}

	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatalf("set deadline: %v", err)
	}
	srvTLS := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}})
	if err := srvTLS.Handshake(); err != nil {
		t.Fatalf("server-side handshake: %v", err)
	}
	got := make([]byte, 4)
	if _, err := io.ReadFull(srvTLS, got); err != nil {
		t.Fatalf("read pong: %v", err)
	}
	if string(got) != "pong" {
		t.Fatalf("got %q, want %q", got, "pong")
	}

	if err := <-errCh; err != nil {
		t.Fatalf("upstream: %v", err)
	}
}
