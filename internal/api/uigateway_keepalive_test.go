// Copyright 2025 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package api

import (
	"bufio"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// a relayed WebSocket keeps its run alive

// upgradeBackend is the "sandbox app" half of the relayed-upgrade harness: it
// answers ONE request with 101 Switching Protocols over a hijacked connection
// and then holds it open, which is exactly what a code editor's live-reload or
// LSP socket does. The gateway relays it as a single inbound HTTP request that
// never ends — the shape the per-request TouchRun could not see.
func upgradeBackend(t *testing.T, held <-chan struct{}) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Error("backend ResponseWriter is not a Hijacker")
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Errorf("backend hijack: %v", err)
			return
		}
		if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")); err != nil {
			t.Errorf("backend write 101: %v", err)
		}
		// Hold the socket for the life of the test, then let the relay unwind.
		<-held
		_ = conn.Close()
	})
}

// TestUIGateway_RelayedWebSocketKeepsTheRunAlive pins the relay touched
// updated_at per INBOUND REQUEST, and a relayed WebSocket is one request for
// its whole life — so under auto_stop_after_sec (3600 in the shipped recordmode
// example) the idle reaper stopped the run out from under an editor a human was
// actively typing in. Attach and both SSH lanes already run a keepalive for
// exactly this; the relay was the outlier.
//
// Negative controls: TestUIGateway_RelayTouchesTheRun (the per-request touch is
// unchanged) and TestUIGateway_PerRunConnectionCap (the keepalive rides the
// connection, it does not take a slot of its own).
func TestUIGateway_RelayedWebSocketKeepsTheRunAlive(t *testing.T) {
	held := make(chan struct{})
	defer close(held)
	h := newUIHarness(t, upgradeBackend(t, held))
	// The production ticker is 30s. The keepalive under test is the same
	// goroutine either way; only the wait is shortened.
	h.srv.keepaliveEvery = 15 * time.Millisecond

	gw := httptest.NewServer(h.gateway)
	defer gw.Close()
	cookie := h.openSession()

	conn, err := net.Dial("tcp", gw.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial gateway: %v", err)
	}
	defer conn.Close()
	req, err := http.NewRequest(http.MethodGet, gw.URL+uiRelayPrefix(h.run.ID, "code")+"/ide", nil)
	if err != nil {
		t.Fatalf("build upgrade request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.AddCookie(cookie)
	if err := req.Write(conn); err != nil {
		t.Fatalf("write upgrade request: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), req)
	if err != nil {
		t.Fatalf("read upgrade response: %v", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		t.Fatalf("relayed upgrade: status = %d, want 101", resp.StatusCode)
	}

	// One touch is the inbound request's own. More than one can only come from
	// a keepalive running for the life of the relayed connection.
	deadline := time.Now().Add(3 * time.Second)
	for h.store.touchCount() <= 1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := h.store.touchCount(); got <= 1 {
		t.Fatalf("TouchRun called %d time(s) over a live relayed WebSocket, want > 1 — "+
			"the run's idle clock is frozen at the upgrade and the reaper will stop it under the editor", got)
	}
}
