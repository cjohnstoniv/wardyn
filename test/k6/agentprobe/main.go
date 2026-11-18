// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

// Command agentprobe is the kind conformance harness's agent-side probe (scripts/k6-kind-conformance.sh bakes
// it into the harness's agent image). The suite runs it inside a run's agent container, so every
// request it makes is an agent request through the run's proxy, which it finds in HTTPS_PROXY.
//
//	agentprobe connect HOST:PORT   CONNECT through the proxy; prints the proxy's status line and body
//	agentprobe hold URL SECONDS    CONNECT, TLS and a GET of URL; prints how and when the request ended
//	agentprobe h2watch ADDR SNI SECONDS [RELAYKEY]
//	                               no proxy: one TLS connection to ADDR (HTTP/2 when offered), a GET every
//	                               500 ms on it until it fails, then one fresh dial; run in a proxy pod's
//	                               network namespace, it is a proxy's renew connection through the relay.
//	                               RELAYKEY (base64, 32 bytes) is the run's relay key: every dial opens with
//	                               the relay preamble carrying it, as a keyed proxy's does
package main

import (
	"bufio"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"golang.org/x/net/http2"
)

func main() {
	if len(os.Args) < 3 {
		fail("usage: agentprobe connect HOST:PORT | agentprobe hold URL SECONDS | agentprobe h2watch ADDR SNI SECONDS [RELAYKEY]")
	}
	if os.Args[1] == "h2watch" && (len(os.Args) == 5 || len(os.Args) == 6) {
		key := ""
		if len(os.Args) == 6 {
			key = os.Args[5]
		}
		h2watch(os.Args[2], os.Args[3], os.Args[4], key)
		return
	}
	proxy := os.Getenv("HTTPS_PROXY")
	if proxy == "" {
		proxy = os.Getenv("https_proxy")
	}
	pu, err := url.Parse(proxy)
	if err != nil || pu.Host == "" {
		fail(fmt.Sprintf("no proxy in HTTPS_PROXY (%q)", proxy))
	}
	switch os.Args[1] {
	case "connect":
		c, resp := connect(pu.Host, os.Args[2])
		defer c.Close()
		if resp.StatusCode == http.StatusOK {
			fmt.Println("status 200") // a tunnel: nothing to read before closing it
			return
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		fmt.Printf("status %d\n%s\n", resp.StatusCode, body)
	case "hold":
		if len(os.Args) < 4 {
			fail("hold needs URL SECONDS")
		}
		hold(pu.Host, os.Args[2], os.Args[3])
	default:
		fail("unknown mode " + os.Args[1])
	}
}

// connect opens a CONNECT tunnel to target through the proxy and returns the connection and the
// proxy's answer.
func connect(proxy, target string) (net.Conn, *http.Response) {
	c, err := net.DialTimeout("tcp", proxy, 10*time.Second)
	if err != nil {
		fail("dial proxy: " + err.Error())
	}
	_ = c.SetDeadline(time.Now().Add(30 * time.Second))
	fmt.Fprintf(c, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n\r\n", target, target)
	br := bufio.NewReader(c)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		fail("read CONNECT answer: " + err.Error())
	}
	_ = c.SetDeadline(time.Time{})
	return &bufConn{Conn: c, r: br}, resp
}

type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// hold sends one GET through a CONNECT tunnel and waits up to seconds for its end, printing
// "started" once the request is written and then "ended after <d>: <how>".
func hold(proxy, raw, seconds string) {
	u, err := url.Parse(raw)
	if err != nil {
		fail(err.Error())
	}
	secs, err := strconv.Atoi(seconds)
	if err != nil {
		fail(err.Error())
	}
	port := u.Port()
	if port == "" {
		port = "443"
	}
	c, resp := connect(proxy, net.JoinHostPort(u.Hostname(), port))
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		fmt.Printf("connect status %d\n%s\n", resp.StatusCode, body)
		return
	}
	// The probe judges the proxy, not the upstream's certificate, which the proxy re-signs.
	tc := tls.Client(c, &tls.Config{ServerName: u.Hostname(), InsecureSkipVerify: true}) //nolint:gosec // see above
	_ = tc.SetDeadline(time.Now().Add(time.Duration(secs) * time.Second))
	start := time.Now()
	if err := tc.Handshake(); err != nil {
		fmt.Printf("ended after %s: handshake: %v\n", time.Since(start).Round(time.Millisecond), err)
		return
	}
	fmt.Fprintf(tc, "GET %s HTTP/1.1\r\nHost: %s\r\n\r\n", u.RequestURI(), u.Host)
	fmt.Println("started")
	r, err := http.ReadResponse(bufio.NewReader(tc), nil)
	if err != nil {
		fmt.Printf("ended after %s: %v\n", time.Since(start).Round(time.Millisecond), err)
		return
	}
	fmt.Printf("ended after %s: status %d\n", time.Since(start).Round(time.Millisecond), r.StatusCode)
}

// h2watch prints "proto P" once connected, "ok N" per answered GET, then "cut after D: ERR" when the
// connection fails and "redial: RESULT" for one new connection from the same address. A non-empty
// relayKey makes every connection open with the relay preamble, which the relay answers by closing a
// connection whose key is not the run's own: the dial then fails and "proto" is never printed.
func h2watch(addr, sni, seconds, relayKey string) {
	secs, err := strconv.Atoi(seconds)
	if err != nil {
		fail(err.Error())
	}
	var key []byte
	if relayKey != "" {
		if key, err = base64.StdEncoding.DecodeString(relayKey); err != nil || len(key) != relayKeyLen {
			fail("relay key is not 32 bytes of base64")
		}
	}
	// The probe judges the relay's admission, not the organisation's certificate.
	cfg := &tls.Config{ServerName: sni, NextProtos: []string{"h2", "http/1.1"}, InsecureSkipVerify: true} //nolint:gosec // see above
	dial := func() (*tls.Conn, error) { return dialRelay(addr, cfg, key) }
	tc, err := dial()
	if err != nil {
		fail("dial: " + err.Error())
	}
	proto := tc.ConnectionState().NegotiatedProtocol
	fmt.Println("proto", proto)
	var rt func(*http.Request) (*http.Response, error)
	if proto == "h2" {
		//lint:ignore SA1019 the probe speaks HTTP/2 on the TLS connection it already dialled; net/http's replacement dials its own.
		cc, err := (&http2.Transport{}).NewClientConn(tc)
		if err != nil {
			fail("h2: " + err.Error())
		}
		rt = cc.RoundTrip
	} else {
		br := bufio.NewReader(tc)
		rt = func(r *http.Request) (*http.Response, error) {
			if err := r.Write(tc); err != nil {
				return nil, err
			}
			return http.ReadResponse(br, r)
		}
	}
	start := time.Now()
	for time.Since(start) < time.Duration(secs)*time.Second {
		req, _ := http.NewRequest(http.MethodGet, "https://"+sni+"/healthz", nil)
		_ = tc.SetDeadline(time.Now().Add(5 * time.Second))
		resp, err := rt(req)
		if err != nil {
			fmt.Printf("cut after %s: %v\n", time.Since(start).Round(time.Millisecond), err)
			c, derr := dial()
			if derr != nil {
				fmt.Println("redial:", derr)
			} else {
				fmt.Println("redial: connected")
				_ = c.Close()
			}
			return
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		fmt.Println("ok", resp.StatusCode)
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Println("still open after", seconds, "s")
}

func fail(msg string) {
	fmt.Fprintln(os.Stderr, "agentprobe: "+msg)
	os.Exit(2)
}

// relayKeyLen, relayPreambleMagic and relayPreambleVersion are the relay preamble's wire format as
// internal/egress/proxy writes it: magic, version, then the 32-byte key, before the TLS ClientHello.
const (
	relayKeyLen          = 32
	relayPreambleMagic   = 0x57
	relayPreambleVersion = 0x01
)

// dialRelay dials addr and, when key is set, writes the relay preamble before the TLS handshake.
func dialRelay(addr string, cfg *tls.Config, key []byte) (*tls.Conn, error) {
	if key == nil {
		return tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", addr, cfg)
	}
	c, err := (&net.Dialer{Timeout: 5 * time.Second}).Dial("tcp", addr)
	if err != nil {
		return nil, err
	}
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err := c.Write(append([]byte{relayPreambleMagic, relayPreambleVersion}, key...)); err != nil {
		_ = c.Close()
		return nil, err
	}
	tc := tls.Client(c, cfg)
	if err := tc.Handshake(); err != nil {
		_ = c.Close()
		return nil, err
	}
	_ = c.SetDeadline(time.Time{})
	return tc, nil
}
