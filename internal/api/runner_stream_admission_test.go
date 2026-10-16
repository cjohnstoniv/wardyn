// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"fmt"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/coder/websocket"
)

var loopbackProxies = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

// Forged attempts name runner A from many client addresses while A keeps
// proving its key. A's reconnect budget is charged only after proof, so A
// reconnects its full burst however many forgeries arrive in between.
func TestRunnerAdmissionForgedAttemptsDoNotSpendVictimBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1, loopbackProxies...)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	for id, key := range h.keys {
		for i := range int(enrolBurst) {
			for f := range 3 {
				xff := fmt.Sprintf("198.51.100.%d", 10*i+f+1)
				if status, err := h.dialFrom(t, ctx, id, wrong, xff); status != http.StatusSwitchingProtocols || err == nil {
					t.Fatalf("forgery %d/%d from %s: status %d, err %v", i, f, xff, status, err)
				}
			}
			if status, err := h.dialFrom(t, ctx, id, key, "192.0.2.10"); status != http.StatusSwitchingProtocols || err != nil {
				t.Fatalf("runner A connect %d among forgeries: status %d, err %v", i, status, err)
			}
		}
	}
}

// Behind a trusted proxy each client has its own address bucket: one forger
// exhausting its bucket does not refuse the runner behind the same proxy.
func TestRunnerAdmissionTrustedProxySeparatesAddresses(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1, loopbackProxies...)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	for id, key := range h.keys {
		for range int(enrolBurst) {
			_, _ = h.dialFrom(t, ctx, id, wrong, "198.51.100.1")
		}
		if status, _ := h.dialFrom(t, ctx, id, wrong, "198.51.100.1"); status != http.StatusTooManyRequests {
			t.Fatalf("forger past its burst: status %d, want 429", status)
		}
		if status, err := h.dialFrom(t, ctx, id, key, "1.2.3.4, 198.51.100.1, 192.0.2.10"); status != http.StatusSwitchingProtocols || err != nil {
			t.Fatalf("runner behind the same proxy: status %d, err %v", status, err)
		}
	}
}

// With no trusted proxy, a rotating X-Forwarded-For buys no fresh bucket.
func TestRunnerAdmissionIgnoresForwardedForFromUntrustedPeer(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1)
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	for id := range h.keys {
		for i := range int(enrolBurst) {
			_, _ = h.dialFrom(t, ctx, id, wrong, fmt.Sprintf("198.51.100.%d", i+1))
		}
		if status, _ := h.dialFrom(t, ctx, id, wrong, "198.51.100.200"); status != http.StatusTooManyRequests {
			t.Fatalf("spoofed X-Forwarded-For past the burst: status %d, want 429", status)
		}
	}
}

// The global bucket refuses every address once anonymous failures spend it.
func TestRunnerAdmissionGlobalBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1, loopbackProxies...)
	h.srv.runnerStreams.global = principalLimiter{rate: 1, burst: 3, max: 1}
	_, wrong, _ := ed25519.GenerateKey(rand.Reader)
	for id, key := range h.keys {
		for i := range 3 {
			if status, _ := h.dialFrom(t, ctx, id, wrong, fmt.Sprintf("198.51.100.%d", i+1)); status != http.StatusSwitchingProtocols {
				t.Fatalf("failure %d within the global burst: status %d", i, status)
			}
		}
		if status, _ := h.dialFrom(t, ctx, id, key, "192.0.2.10"); status != http.StatusTooManyRequests {
			t.Fatalf("fresh address past the global burst: status %d, want 429", status)
		}
	}
}

// Connections held open without AUTH occupy handshake slots; past the cap a
// new connection is refused, and finishing one frees its slot.
func TestRunnerHandshakeConcurrencyCap(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	h := newStreamLimitHarness(t, ctx, 1)
	h.srv.runnerStreams.maxHandshakes = 2
	for id, key := range h.keys {
		var held []*websocket.Conn
		for i := range 2 {
			conn, _, err := websocket.Dial(ctx, h.url+"/api/v1/runners/"+id.String()+"/stream", nil)
			if err != nil {
				t.Fatalf("held handshake %d: %v", i, err)
			}
			defer conn.CloseNow()
			held = append(held, conn)
		}
		if status, _ := h.dial(t, ctx, id, key); status != http.StatusTooManyRequests {
			t.Fatalf("handshake past the cap: status %d, want 429", status)
		}
		held[0].CloseNow()
		select {
		case <-h.handled:
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
		if status, err := h.dial(t, ctx, id, key); status != http.StatusSwitchingProtocols || err != nil {
			t.Fatalf("after a slot freed: status %d, err %v", status, err)
		}
		if n := h.srv.runnerStreams.handshakes.Load(); n != 1 {
			t.Fatalf("handshakes in progress = %d, want the one still held", n)
		}
	}
}
