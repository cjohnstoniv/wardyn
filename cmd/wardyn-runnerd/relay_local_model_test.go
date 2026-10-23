// Copyright 2026 The Wardyn Authors
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"io"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/cjohnstoniv/wardyn/internal/runnerwire"
)

// A laptop local model's port (#2957) is served at the relay's bind and forwarded to the model
// server's address at that port alone; asked again it is the same listener. The organisation relay's
// own port is refused, and where the bind is the model server's own address nothing is opened.
func TestLocalModelRelayForwardsItsPortToTheModelServer(t *testing.T) {
	model, err := net.Listen("tcp", "127.0.0.2:0") // the model server, on another loopback address than the bind
	if err != nil {
		t.Skipf("no 127.0.0.2 on this host: %v", err)
	}
	defer model.Close()
	go func() {
		for {
			c, err := model.Accept()
			if err != nil {
				return
			}
			go func() { _, _ = io.Copy(c, c); _ = c.Close() }()
		}
	}()
	port := uint16(model.Addr().(*net.TCPAddr).Port)
	r := &localRelay{ctx: context.Background(), pair: pair("127.0.0.1", "host-gateway"), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{},
		modelAddr: netip.MustParseAddr("127.0.0.2")}
	defer r.Close()
	for range 2 {
		if err := r.EnsureLocalModel(port); err != nil {
			t.Fatalf("EnsureLocalModel: %v", err)
		}
	}
	if len(r.models) != 1 {
		t.Fatalf("listeners %v, want one", r.models)
	}
	c, err := net.Dial("tcp", netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), port).String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := c.Write([]byte("POST /api/chat")); err != nil {
		t.Fatal(err)
	}
	_ = c.(*net.TCPConn).CloseWrite()
	if got, _ := io.ReadAll(c); string(got) != "POST /api/chat" {
		t.Fatalf("the model server answered %q through the relay", got)
	}

	r.listeners[443] = model // stands in for the organisation relay's listener on 443
	if err := r.EnsureLocalModel(443); err == nil || !strings.Contains(err.Error(), "organisation relay's own") {
		t.Errorf("the organisation relay's port = %v", err)
	}
	delete(r.listeners, 443)
	if _, err := r.Ensure(&url.URL{Scheme: "https", Host: "org.example:" + strconv.Itoa(int(port))}); err == nil {
		t.Error("the organisation relay took a local model server's port")
	}

	direct := &localRelay{ctx: context.Background(), pair: pair("127.0.0.1", "host-gateway"), listeners: map[uint16]net.Listener{}, conns: map[net.Conn]*runnerwire.Peer{}}
	if err := direct.EnsureLocalModel(11434); err != nil || len(direct.models) != 0 {
		t.Errorf("a loopback bind opened %v (%v): the model server answers there itself", direct.models, err)
	}
}
