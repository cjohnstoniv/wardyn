// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cjohnstoniv/wardyn/internal/egress"
	"github.com/cjohnstoniv/wardyn/internal/types"
)

// echoListener accepts connections and echoes them until the test ends.
func echoListener(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	return ln.Addr().String()
}

// openCONNECT sends one CONNECT to the proxy at addr and returns the client
// leg (closed at the test's end) and the proxy's answer.
func openCONNECT(t *testing.T, addr string) (net.Conn, *http.Response) {
	t.Helper()
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	if _, err := io.WriteString(c, "CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatalf("read CONNECT answer: %v", err)
	}
	_ = c.SetReadDeadline(time.Time{})
	return c, resp
}

// waitTunnels waits until the proxy counts want open tunnels.
func waitTunnels(t *testing.T, p *Proxy, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for p.openTunnels.Load() != want {
		if time.Now().After(deadline) {
			t.Fatalf("open tunnels = %d, want %d", p.openTunnels.Load(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTunnelCap_RefusesPastTheCapUntilOneCloses: a run holds at most
// maxOpenTunnels opaque tunnels. The next is a temporary 503 with a retry
// hint and the builtin:tunnel-limit reason and deny row, never
// X-Wardyn-Egress: denied; once one tunnel closes, the next is admitted.
func TestTunnelCap_RefusesPastTheCapUntilOneCloses(t *testing.T) {
	p, buf := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, echoListener(t), nil, nil)
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()

	conns := make([]net.Conn, 0, maxOpenTunnels)
	for i := range maxOpenTunnels {
		c, resp := openCONNECT(t, addr)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tunnel %d = %d, want 200 under the cap", i+1, resp.StatusCode)
		}
		conns = append(conns, c)
	}
	_, resp := openCONNECT(t, addr)
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(egressHeaderReason) != ruleSourceTunnelLimit ||
		resp.Header.Get("Retry-After") == "" || resp.Header.Get("X-Should-Retry") != "true" || resp.Header.Get(egressHeaderHost) != "example.com" {
		t.Fatalf("tunnel %d = %d %v, want a 503 naming %s and example.com with a retry hint", maxOpenTunnels+1, resp.StatusCode, resp.Header, ruleSourceTunnelLimit)
	}
	if got := resp.Header.Get(egressHeaderStatus); got != "" {
		t.Errorf("%s = %q on a temporary refusal, want none: denied means permanently blocked", egressHeaderStatus, got)
	}
	if d := lastDecision(t, buf); d.Decision != egress.Deny || d.RuleSource != ruleSourceTunnelLimit || d.Request.Host != "example.com" {
		t.Errorf("decision = %+v, want a %s deny for example.com", d, ruleSourceTunnelLimit)
	}

	_ = conns[0].Close()
	waitTunnels(t, p, maxOpenTunnels-1)
	if _, resp := openCONNECT(t, addr); resp.StatusCode != http.StatusOK {
		t.Fatalf("after one tunnel closed = %d, want 200", resp.StatusCode)
	}
}

// TestTunnelCap_FailedDialKeepsNoSlot: a dial that fails gives its slot back,
// so it never counts against the cap.
func TestTunnelCap_FailedDialKeepsNoSlot(t *testing.T) {
	echo := echoListener(t)
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, echo, nil, nil)
	p.maxTunnels = 1
	var dials atomic.Int32
	p.dial = func(ctx context.Context, network, _ string) (net.Conn, error) {
		if dials.Add(1) <= 3 {
			return nil, errors.New("dial refused")
		}
		return (&net.Dialer{}).DialContext(ctx, network, echo)
	}
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()

	for i := range 3 {
		if _, resp := openCONNECT(t, addr); resp.StatusCode != http.StatusBadGateway {
			t.Fatalf("failed dial %d = %d, want 502", i+1, resp.StatusCode)
		}
	}
	if _, resp := openCONNECT(t, addr); resp.StatusCode != http.StatusOK {
		t.Fatalf("tunnel after three failed dials = %d, want 200: a failed dial kept its slot", resp.StatusCode)
	}
	if _, resp := openCONNECT(t, addr); resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("tunnel past a cap of 1 = %d, want 503", resp.StatusCode)
	}
}

// TestTunnelCap_DrainReleasesTheSlots: a runner sink's drain closes the open
// tunnels, and each one's slot is given back.
func TestTunnelCap_DrainReleasesTheSlots(t *testing.T) {
	rs, f := startedSink(t)
	c := accept(t, rs, f.next(), 1, 0, 10, 1<<20)
	p, _ := newTestProxy(t, types.RunPolicySpec{AllowAllEgress: true}, echoListener(t), nil, nil)
	p.sink = &decisionSink{remote: rs}
	p.maxTunnels = 2
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()

	var clients []net.Conn
	for i := range 2 {
		cl, resp := openCONNECT(t, addr)
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("tunnel %d = %d, want 200", i+1, resp.StatusCode)
		}
		clients = append(clients, cl)
	}
	waitTunnels(t, p, 2)
	writeFrame(t, c, SinkFrame{T: FrameDrain, Gen: 1})
	readUntil(t, c, FrameDrained)
	for _, cl := range clients {
		_ = cl.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := cl.Read(make([]byte, 1)); !errors.Is(err, io.EOF) {
			t.Fatalf("tunnel still open after the drain: %v", err)
		}
	}
	waitTunnels(t, p, 0)
}

// TestTunnelCap_ResumedRelayFlowsLeaveTheSigninLane: a relay flow resumed from
// its ask holds the relay's slot, not the proxy's, so a VM guest at the relay's
// bound cannot starve the person's sign-in CONNECT.
func TestTunnelCap_ResumedRelayFlowsLeaveTheSigninLane(t *testing.T) {
	rig := newSigninLaneRigFor(t, true, func(o *Options) {
		o.Policy = CompilePolicy(types.RunPolicySpec{AllowedDomains: []string{"site.test", "relay.test"}})
	})
	for i := range maxOpenTunnels {
		flow := fmt.Sprintf("%016x", i+1)
		if st, _, _ := stagedConnect(t, rig.agentAddr, "relay.test:443", flow, vmnetStageAsk); st != http.StatusNoContent {
			t.Fatalf("relay ask %d = %d, want 204", i+1, st)
		}
		if st, _, _ := stagedConnect(t, rig.agentAddr, "relay.test:443", flow, vmnetStageTunnel); st != http.StatusOK {
			t.Fatalf("relay tunnel %d = %d, want 200", i+1, st)
		}
	}
	if _, _, st := rig.connect(t, rig.signinAddr, "site.test:443", signinAuth(rig.goodCred)); st != http.StatusOK {
		t.Fatalf("sign-in CONNECT beside %d relay flows = %d, want 200", maxOpenTunnels, st)
	}
}

// TestTunnelCap_ForgedStageIsCounted: a tunnel-stage header with no parked flow
// behind it, which a guest can send, is evaluated afresh and takes a slot.
func TestTunnelCap_ForgedStageIsCounted(t *testing.T) {
	p, _ := newRuleTestProxy(t, types.RunPolicySpec{AllowedDomains: []string{"origin.test"}}, echoListener(t), true, "", nil)
	p.maxTunnels = 1
	srv := httptest.NewServer(p)
	t.Cleanup(srv.Close)
	addr := srv.Listener.Addr().String()

	if st, _, _ := stagedConnect(t, addr, "origin.test:443", flowA, vmnetStageTunnel); st != http.StatusOK {
		t.Fatalf("forged tunnel stage = %d, want 200 under the cap", st)
	}
	c, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	_, _ = fmt.Fprintf(c, "CONNECT origin.test:443 HTTP/1.1\r\nHost: origin.test:443\r\n%s: %s\r\n%s: %s\r\n\r\n",
		vmnetFlowHeader, flowB, vmnetStageHeader, vmnetStageTunnel)
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), &http.Request{Method: http.MethodConnect})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get(egressHeaderReason) != ruleSourceTunnelLimit ||
		resp.Header.Get(egressHeaderHost) != "origin.test" {
		t.Fatalf("second forged tunnel stage = %d %v, want a 503 naming %s and origin.test", resp.StatusCode, resp.Header, ruleSourceTunnelLimit)
	}
}
