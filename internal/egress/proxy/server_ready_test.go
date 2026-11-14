// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/google/uuid"
)

// OnListening runs once the listener is bound, which is the moment a runner may start the agent: the
// address must already accept a connection when the hook fires.
func TestServer_OnListeningFiresOnceTheListenerIsBound(t *testing.T) {
	b, err := json.Marshal(&Config{RunID: uuid.New(), ControlPlaneURL: "http://127.0.0.1:1", RunToken: "tok", Listen: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfigBytes(b)
	if err != nil {
		t.Fatalf("LoadConfigBytes: %v", err)
	}
	srv, err := NewServer(context.Background(), cfg, &http.Client{Timeout: time.Second}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	fired := make(chan struct{})
	var bound net.Addr
	srv.OnListening(func() { bound = srv.BoundAddr(); close(fired) })
	go func() { _ = srv.ListenAndServe() }()
	select {
	case <-fired:
	case <-time.After(5 * time.Second):
		t.Fatal("OnListening never fired")
	}
	// BoundAddr is set before the hook runs and names the port the kernel chose for the ":0" listen.
	if tcp, ok := bound.(*net.TCPAddr); !ok || tcp.Port == 0 {
		t.Fatalf("BoundAddr in the hook = %v, want the bound loopback port", bound)
	}
	c, err := net.Dial("tcp", bound.String())
	if err != nil {
		t.Fatalf("the hook's BoundAddr does not accept a connection: %v", err)
	}
	_ = c.Close()
}
