// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package vmnet

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// The relay's side of the proxy contract (internal/egress/proxy/vmnet_flows.go):
// an ask at the guest's SYN, a tunnel once the first flight passed, and a
// refusal report for what only the relay sees. The headers and the route must
// match the proxy's.
const (
	flowHeader   = "Wardyn-Vmnet-Flow"
	stageHeader  = "Wardyn-Vmnet-Stage"
	refusalsPath = "/wardyn/v1/vmnet/refusals"

	// askBudget bounds an ask, which a first-use approval may hold: inside
	// Linux's ~127 s SYN-retry budget and Firefox's 90 s connect timeout. A
	// longer first_use_hold_seconds is cut here and the guest sees a reset.
	askBudget  = 60 * time.Second
	dialBudget = 5 * time.Second
)

type proxyClient struct {
	addr   string // host:port of the run's wardyn-proxy
	dial   func(ctx context.Context, network, addr string) (net.Conn, error)
	report *http.Client
}

func newProxyClient(addr string, dial func(ctx context.Context, network, addr string) (net.Conn, error)) *proxyClient {
	if dial == nil {
		d := &net.Dialer{Timeout: dialBudget}
		dial = d.DialContext
	}
	return &proxyClient{
		addr: addr, dial: dial,
		report: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			Proxy: nil, DialContext: func(ctx context.Context, n, _ string) (net.Conn, error) { return dial(ctx, n, addr) },
		}},
	}
}

// connect sends one staged CONNECT and returns the open connection with the
// proxy's status. The connection is nil unless the status is 2xx.
func (c *proxyClient) connect(name string, port uint16, flow, stage string, budget time.Duration) (net.Conn, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), dialBudget)
	pc, err := c.dial(ctx, "tcp", c.addr)
	cancel()
	if err != nil {
		return nil, 0, err
	}
	target := net.JoinHostPort(name, strconv.Itoa(int(port)))
	_ = pc.SetDeadline(time.Now().Add(budget))
	if _, err := fmt.Fprintf(pc, "CONNECT %s HTTP/1.1\r\nHost: %s\r\n%s: %s\r\n%s: %s\r\n\r\n",
		target, target, flowHeader, flow, stageHeader, stage); err != nil {
		_ = pc.Close()
		return nil, 0, err
	}
	br := bufio.NewReader(pc)
	resp, err := http.ReadResponse(br, &http.Request{Method: http.MethodConnect})
	if err != nil {
		_ = pc.Close()
		return nil, 0, err
	}
	_ = resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		_ = pc.Close()
		return nil, resp.StatusCode, nil
	}
	_ = pc.SetDeadline(time.Time{})
	return &bufConn{Conn: pc, r: br}, resp.StatusCode, nil
}

// ask asks whether a flow may open; true only when the proxy parked an allow.
func (c *proxyClient) ask(name string, port uint16, flow string) (bool, int, error) {
	pc, status, err := c.connect(name, port, flow, "ask", askBudget)
	if pc != nil {
		_ = pc.Close()
	}
	return pc != nil, status, err
}

// tunnel opens the flow's tunnel through the proxy.
func (c *proxyClient) tunnel(name string, port uint16, flow string) (net.Conn, int, error) {
	return c.connect(name, port, flow, "tunnel", askBudget)
}

// forwarder is the round tripper a port-80 flow's requests go through: the
// proxy as an ordinary forward proxy.
func (c *proxyClient) forwarder() *http.Transport {
	return &http.Transport{
		Proxy:              http.ProxyURL(&url.URL{Scheme: "http", Host: c.addr}),
		DialContext:        c.dial,
		DisableCompression: true,
		MaxIdleConns:       1,
		IdleConnTimeout:    httpIdle,
	}
}

// refusal is one report; the JSON matches the proxy's vmnetRefusal.
type refusal struct {
	Flow   string `json:"flow,omitempty"`
	Reason string `json:"reason"`
	Host   string `json:"host,omitempty"`
	Port   int    `json:"port,omitempty"`
	Detail string `json:"detail,omitempty"`
	Repeat int    `json:"repeat,omitempty"`
}

// send posts one refusal. Best effort: the relay's own log line already
// records it, and a proxy that cannot take it has bigger problems.
func (c *proxyClient) send(r refusal) error {
	body, _ := json.Marshal(r)
	resp, err := c.report.Post("http://"+c.addr+refusalsPath, "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("vmnet: refusal report answered %d", resp.StatusCode)
	}
	return nil
}

// bufConn reads through the reader that parsed the proxy's response, so tunnel
// bytes the proxy sent behind its status line are not lost.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// CloseWrite half-closes the proxy leg when the guest finished sending.
func (b *bufConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}
